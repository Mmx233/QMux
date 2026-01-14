package tools

import "os"

func GetenvDefault(key string, defaultValue string) string {
	value := os.Getenv(key)
	if key == "" {
		return defaultValue
	}
	return value
}
