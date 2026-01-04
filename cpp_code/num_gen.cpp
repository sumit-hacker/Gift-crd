#include <iostream>
#include <fstream>
#include <string>
#include <iomanip>

static const std::string PREFIX = "100134004";
static const int TOTAL_LENGTH = 16;
static const int FILE_BATCH_SIZE = 10000;

// Change this path if needed
static const std::string OUTPUT_DIR = "./luhn_number/";

/**
 * Calculate Luhn check digit for a number string (without check digit)
 */
int calculateLuhnCheckDigit(const std::string& number) {
    int sum = 0;
    bool shouldDouble = true;

    for (int i = number.size() - 1; i >= 0; --i) {
        int digit = number[i] - '0';

        if (shouldDouble) {
            digit *= 2;
            if (digit > 9) digit -= 9;
        }

        sum += digit;
        shouldDouble = !shouldDouble;
    }

    return (10 - (sum % 10)) % 10;
}

/**
 * Increment last digit (9 → 0)
 */
std::string incrementLastDigit(const std::string& number) {
    std::string result = number;
    int lastDigit = result.back() - '0';
    result.back() = char('0' + (lastDigit + 1) % 10);
    return result;
}

int main() {
    const int variableDigits = TOTAL_LENGTH - PREFIX.length() - 1; // 6
    const int maxCombinations = 1;
    for (int i = 0; i < variableDigits; ++i) {
        // 10^6 combinations
    }

    int fileIndex = 1;
    int countInFile = 0;
    long long totalGenerated = 0;

    std::ofstream outFile;

    auto openNewFile = [&]() {
        if (outFile.is_open()) outFile.close();
        std::string fileName = OUTPUT_DIR + "luhn_numbers_" + std::to_string(fileIndex++) + ".txt";
        outFile.open(fileName);
        countInFile = 0;
    };

    openNewFile();

    // Iterate through all 6-digit combinations: 000000 → 999999
    for (int i = 0; i < 1'000'000; ++i) {
        std::ostringstream oss;
        oss << PREFIX << std::setw(variableDigits) << std::setfill('0') << i;
        std::string baseNumber = oss.str();

        int checkDigit = calculateLuhnCheckDigit(baseNumber);
        std::string luhnValid = baseNumber + char('0' + checkDigit);

        // Apply +1 rule
        std::string finalNumber = incrementLastDigit(luhnValid);

        outFile << finalNumber << '\n';
        ++countInFile;
        ++totalGenerated;

        if (countInFile == FILE_BATCH_SIZE) {
            openNewFile();
        }
    }

    if (outFile.is_open()) outFile.close();

    std::cout << "Generation completed.\n";
    std::cout << "Total numbers generated: " << totalGenerated << std::endl;
    std::cout << "Files created: " << (fileIndex - 1) << std::endl;

    return 0;
}
