package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/FuzzyHD1993/nas-stack/internal/platform"
	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/adaptor"
	"github.com/gofiber/fiber/v3/middleware/limiter"
	"github.com/gofiber/fiber/v3/middleware/recover"
	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const (
	tokenIssuer = "nas-auth"
	scopeAdmin  = "admin"
	defaultTTL  = 7 * 24 * time.Hour
	maxTokenTTL = 30 * 24 * time.Hour
)

type config struct {
	password    string
	jwtSecret   []byte
	authEnabled bool
	tokenTTL    time.Duration
}

func main() {
	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("❌ %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	db := connectProfilesDB(ctx)
	cancel()
	if db != nil {
		defer db.Close()
	}

	app := newApp(cfg, db)
	platform.StartPprof()

	listenErr := make(chan error, 1)
	go func() {
		log.Println("🔐 auth-service на :8086")
		if err := app.Listen(":8086", fiber.ListenConfig{DisableStartupMessage: true}); err != nil {
			listenErr <- err
		}
	}()

	sig, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	select {
	case <-sig.Done():
	case err := <-listenErr:
		log.Printf("listen: %v", err)
	}

	if err := app.ShutdownWithTimeout(10 * time.Second); err != nil {
		log.Printf("shutdown: %v", err)
	}
}

func loadConfig() (config, error) {
	cfg := config{
		authEnabled: strings.EqualFold(strings.TrimSpace(os.Getenv("AUTH_ENABLED")), "true"),
		tokenTTL:    defaultTTL,
	}

	if v := strings.TrimSpace(os.Getenv("AUTH_TOKEN_TTL")); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 || d > maxTokenTTL {
			return cfg, fmt.Errorf("AUTH_TOKEN_TTL: ожидается duration (например 168h), получено %q", v)
		}
		cfg.tokenTTL = d
	}

	secret := strings.TrimSpace(os.Getenv("JWT_SECRET"))
	weakSecret := len(secret) < 32 || secret == "change-me-in-production"
	if weakSecret {
		if cfg.authEnabled {
			return cfg, errors.New("JWT_SECRET не задан, короче 32 байт или равен дефолту — сгенерируйте: openssl rand -hex 32")
		}
		var err error
		if secret, err = randomHex(32); err != nil {
			return cfg, err
		}
		log.Println("⚠️ JWT_SECRET не задан/слабый — auth выключен, секрет сгенерирован на время работы")
	}
	cfg.jwtSecret = []byte(secret)

	password := os.Getenv("AUTH_PASSWORD")
	if cfg.authEnabled {
		if password == "" || password == "admin" {
			return cfg, errors.New("AUTH_PASSWORD не задан или равен 'admin' — задайте пароль в .env")
		}
		if len(password) < 8 {
			return cfg, errors.New("AUTH_PASSWORD короче 8 символов")
		}
		if len(password) > 1024 {
			return cfg, errors.New("AUTH_PASSWORD длиннее 1024 байт")
		}
		cfg.password = password
	} else {
		log.Println("⚠️ AUTH_ENABLED=false — /login отключён, профили без авторизации")
	}

	return cfg, nil
}

func newApp(cfg config, db *pgxpool.Pool) *fiber.App {
	app := fiber.New(fiber.Config{
		AppName:      "auth-service",
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  60 * time.Second,
		BodyLimit:    64 * 1024,
	})
	app.Use(recover.New())
	app.Use(securityHeaders)
	app.Use(metricsMiddleware())

	app.Get("/metrics", adaptor.HTTPHandler(promhttp.Handler()))
	app.Get("/health", healthHandler(db))

	app.Post("/login", loginLimiter(), loginHandler(cfg))
	app.Get("/verify", verifyHandler(cfg))

	profileRoutes(app, db, cfg)
	return app
}

func securityHeaders(c fiber.Ctx) error {
	c.Set("X-Content-Type-Options", "nosniff")
	c.Set("Referrer-Policy", "no-referrer")
	return c.Next()
}

func healthHandler(db *pgxpool.Pool) fiber.Handler {
	return func(c fiber.Ctx) error {
		if db == nil {
			m := platform.HealthJSON("auth", fiber.Map{"db": "down"})
			m["status"] = "degraded"
			return c.Status(fiber.StatusServiceUnavailable).JSON(m)
		}
		ctx, cancel := context.WithTimeout(c.Context(), 2*time.Second)
		defer cancel()
		if err := db.Ping(ctx); err != nil {
			m := platform.HealthJSON("auth", fiber.Map{"db": "down"})
			m["status"] = "degraded"
			return c.Status(fiber.StatusServiceUnavailable).JSON(m)
		}
		return c.JSON(platform.HealthJSON("auth", fiber.Map{"db": "ok"}))
	}
}

func loginLimiter() fiber.Handler {
	return limiter.New(limiter.Config{
		Max:          10,
		Expiration:   time.Minute,
		KeyGenerator: clientKey,
		LimitReached: func(c fiber.Ctx) error {
			return c.Status(fiber.StatusTooManyRequests).JSON(fiber.Map{"error": "too many attempts"})
		},
	})
}

func loginHandler(cfg config) fiber.Handler {
	return func(c fiber.Ctx) error {
		if !cfg.authEnabled {
			return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "auth disabled"})
		}

		var body struct {
			Password string `json:"password"`
		}
		if err := c.Bind().JSON(&body); err != nil || len(body.Password) > 1024 {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid body"})
		}

		if !constantTimeEqual(body.Password, cfg.password) {
			log.Printf("login failed ip=%s", clientKey(c))
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "invalid password"})
		}

		token, err := issueToken(cfg.jwtSecret, scopeAdmin, 0, cfg.tokenTTL)
		if err != nil {
			log.Printf("login: sign token: %v", err)
			return internalError(c)
		}

		c.Set("Cache-Control", "no-store")
		c.Set("Pragma", "no-cache")
		return c.JSON(fiber.Map{
			"token":      token,
			"expires_in": int64(cfg.tokenTTL.Seconds()),
		})
	}
}

func verifyHandler(cfg config) fiber.Handler {
	return func(c fiber.Ctx) error {
		if !cfg.authEnabled {
			return c.JSON(fiber.Map{"valid": true})
		}
		claims, err := parseToken(bearerToken(c), cfg.jwtSecret)
		if err != nil {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"valid": false, "error": "invalid token"})
		}
		if scope, _ := claims["scope"].(string); scope != scopeAdmin {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"valid": false, "error": "invalid token"})
		}

		out := fiber.Map{"valid": true, "sub": claims["sub"], "scope": claims["scope"]}
		if pid, ok := claims["profile_id"]; ok {
			out["profile_id"] = pid
		}
		return c.JSON(out)
	}
}

// issueToken — единственная точка выпуска JWT.
func issueToken(secret []byte, scope string, profileID int, ttl time.Duration) (string, error) {
	jti, err := randomHex(16)
	if err != nil {
		return "", err
	}
	now := time.Now()
	claims := jwt.MapClaims{
		"iss":   tokenIssuer,
		"sub":   "user",
		"scope": scope,
		"jti":   jti,
		"iat":   now.Unix(),
		"exp":   now.Add(ttl).Unix(),
	}
	if profileID > 0 {
		claims["profile_id"] = profileID
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(secret)
}

// parseToken — строгая проверка: только HS256, обязательный exp и issuer.
func parseToken(tokenStr string, secret []byte) (jwt.MapClaims, error) {
	if strings.TrimSpace(tokenStr) == "" {
		return nil, jwt.ErrTokenMalformed
	}
	token, err := jwt.Parse(tokenStr, func(t *jwt.Token) (interface{}, error) {
		if t.Method.Alg() != jwt.SigningMethodHS256.Alg() {
			return nil, jwt.ErrSignatureInvalid
		}
		return secret, nil
	},
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithExpirationRequired(),
		jwt.WithIssuer(tokenIssuer),
		jwt.WithIssuedAt(),
	)
	if err != nil {
		return nil, err
	}
	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok || !token.Valid {
		return nil, jwt.ErrTokenInvalidClaims
	}
	return claims, nil
}

func bearerToken(c fiber.Ctx) string {
	auth := strings.TrimSpace(c.Get("Authorization"))
	if len(auth) < 7 || !strings.EqualFold(auth[:7], "Bearer ") {
		return ""
	}
	return strings.TrimSpace(auth[7:])
}

func requireAdmin(cfg config) fiber.Handler {
	return func(c fiber.Ctx) error {
		if !cfg.authEnabled {
			return c.Next()
		}
		claims, err := parseToken(bearerToken(c), cfg.jwtSecret)
		if err != nil {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "invalid token"})
		}
		if scope, _ := claims["scope"].(string); scope != scopeAdmin {
			return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "forbidden"})
		}
		return c.Next()
	}
}

// clientKey — ключ rate-limit. X-Forwarded-For принимаем только от
// приватного/loopback пира (gateway в compose-сети): иначе любой клиент
// подделает IP и обойдёт лимит.
func clientKey(c fiber.Ctx) string {
	peer := strings.TrimSpace(c.IP())

	if !trustForwardedFor(peer) {
		return peer
	}
	if xff := strings.TrimSpace(c.Get("X-Forwarded-For")); xff != "" {
		if i := strings.IndexByte(xff, ','); i >= 0 {
			xff = xff[:i]
		}
		if ip := strings.TrimSpace(xff); net.ParseIP(ip) != nil {
			return ip
		}
	}
	return peer
}

func trustForwardedFor(peer string) bool {
	if strings.EqualFold(strings.TrimSpace(os.Getenv("TRUST_PROXY")), "false") {
		return false
	}
	ip := net.ParseIP(peer)
	return ip != nil && (ip.IsPrivate() || ip.IsLoopback())
}

func internalError(c fiber.Ctx) error {
	return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
}

func constantTimeEqual(a, b string) bool {
	ha := sha256.Sum256([]byte(a))
	hb := sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(ha[:], hb[:]) == 1
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
