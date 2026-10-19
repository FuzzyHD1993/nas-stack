package platform

import (
	"fmt"
	"os"
	"strings"
)

// GetEnv возвращает переменную окружения или значение по умолчанию.
func GetEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// ValidateEnv проверяет наличие обязательных переменных окружения.
// Возвращает понятную пользователю ошибку со списком отсутствующих ключей.
func ValidateEnv(required []string) error {
	var missing []string
	for _, k := range required {
		if strings.TrimSpace(os.Getenv(k)) == "" {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("отсутствуют обязательные переменные: %s — скопируйте .env.example в .env и заполните",
			strings.Join(missing, ", "))
	}
	return nil
}
