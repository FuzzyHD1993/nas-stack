package main

import (
	"context"
	"log"
	"time"

	"github.com/FuzzyHD1993/nas-stack/internal/platform"
	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/adaptor"
	"github.com/golang-jwt/jwt/v5"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var jwtSecret []byte

func main() {
	jwtSecret = []byte(platform.GetEnv("JWT_SECRET", "change-me-in-production"))
	password := platform.GetEnv("AUTH_PASSWORD", "admin")

	if password == "admin" {
		log.Println("⚠️  Используется дефолтный пароль 'admin'. Смени через AUTH_PASSWORD в .env")
	}

	app := fiber.New()
	platform.StartPprof()
	app.Use(metricsMiddleware())

	app.Get("/metrics", adaptor.HTTPHandler(promhttp.Handler()))

	profilesDB := connectProfilesDB(context.Background())
	if profilesDB != nil {
		defer profilesDB.Close()
	}
	profileRoutes(app, profilesDB, jwtSecret)

	app.Get("/health", func(c fiber.Ctx) error { return c.JSON(platform.HealthJSON("auth", nil)) })

	// Логин
	app.Post("/login", func(c fiber.Ctx) error {
		var body struct {
			Password string `json:"password"`
		}
		if err := c.Bind().JSON(&body); err != nil {
			return c.Status(400).JSON(fiber.Map{"error": "invalid body"})
		}

		if body.Password != password {
			return c.Status(401).JSON(fiber.Map{"error": "invalid password"})
		}

		// Генерируем токен на 30 дней
		claims := jwt.MapClaims{
			"sub": "user",
			"iat": time.Now().Unix(),
			"exp": time.Now().Add(30 * 24 * time.Hour).Unix(),
		}
		token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
		signed, err := token.SignedString(jwtSecret)
		if err != nil {
			return c.Status(500).JSON(fiber.Map{"error": err.Error()})
		}

		return c.JSON(fiber.Map{
			"token":      signed,
			"expires_in": 30 * 24 * 3600,
		})
	})

	// Проверка токена
	app.Get("/verify", func(c fiber.Ctx) error {
		auth := c.Get("Authorization")
		if len(auth) < 8 || auth[:7] != "Bearer " {
			return c.Status(401).JSON(fiber.Map{"valid": false, "error": "no token"})
		}
		tokenStr := auth[7:]

		token, err := jwt.Parse(tokenStr, func(t *jwt.Token) (interface{}, error) {
			return jwtSecret, nil
		})
		if err != nil || !token.Valid {
			return c.Status(401).JSON(fiber.Map{"valid": false, "error": "invalid token"})
		}

		return c.JSON(fiber.Map{"valid": true})
	})

	log.Println("🔐 auth-service на :8086")
	log.Fatal(app.Listen(":8086"))
}
