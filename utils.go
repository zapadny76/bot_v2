package utils

import "strings"

// Функция для разделения длинных сообщений на части
func SplitMessage(message string, maxLength int) []string {
	if len(message) <= maxLength {
		return []string{message}
	}

	var chunks []string
	for len(message) > 0 {
		// Находим последний перенос строки в пределах maxLength
		cutIndex := maxLength
		if len(message) > maxLength {
			lastNewLine := strings.LastIndex(message[:maxLength], "\n")
			if lastNewLine > 0 {
				cutIndex = lastNewLine + 1
			}
		}

		chunks = append(chunks, message[:cutIndex])
		message = message[cutIndex:]
	}

	return chunks
}

// Функция для сокращения текста с многоточием
func TruncateText(text string, maxLength int) string {
	if len(text) <= maxLength {
		return text
	}
	return text[:maxLength-3] + "..."
}
