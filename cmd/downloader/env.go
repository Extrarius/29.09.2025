package main

import (
	"os"
	"strconv"
	"time"

	"github.com/joho/godotenv"
)

func init() {
	// Загружаем переменные окружения из .env (если есть).
	// Порядок: .env → .env.local (перекрывает) → файл из ENV_FILE (если задан, перекрывает).
	_ = godotenv.Load(".env")
	_ = godotenv.Overload(".env.local")
	if file := os.Getenv("ENV_FILE"); file != "" {
		_ = godotenv.Overload(file)
	}
}

// env возвращает значение переменной окружения
// или значение по умолчанию, если переменная пуста/не задана.
func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

// envInt читает целое число из переменной окружения,
// иначе возвращает значение по умолчанию.
func envInt(key string, def int) int {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// envDuration читает длительность (например, "5s", "2m")
// из переменной окружения или возвращает значение по умолчанию.
func envDuration(key string, def time.Duration) time.Duration {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}
