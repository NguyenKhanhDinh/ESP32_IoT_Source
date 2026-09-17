package main

import (
	"crypto/rand"
	"crypto/tls"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"github.com/joho/godotenv"

	_ "github.com/lib/pq"

	"golang.org/x/crypto/bcrypt"
)

// ======================================================
// DATA STRUCTURES
// ======================================================

type Telemetry struct {
	Temperature float64 `json:"temperature"`
	Humidity    float64 `json:"humidity"`
	Light       float64 `json:"light"`
}

type LoginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// ======================================================
// DATABASE
// ======================================================

var db *sql.DB

// ======================================================
// LATEST TELEMETRY - RAM CACHE
// ======================================================

var (
	latestData    Telemetry
	hasLatestData bool
	dataMu        sync.RWMutex
)

// Get latest telemetry from RAM
func getLatestData() (Telemetry, bool) {
	dataMu.RLock()
	defer dataMu.RUnlock()

	return latestData, hasLatestData
}

// Update latest telemetry in RAM
func setLatestData(data Telemetry) {
	dataMu.Lock()
	defer dataMu.Unlock()

	latestData = data
	hasLatestData = true
}

// ======================================================
// SSE CLIENTS
// ======================================================

var (
	clients   = make(map[chan Telemetry]struct{})
	clientsMu sync.Mutex
)

// Send new telemetry to all connected SSE clients
func broadcastTelemetry(data Telemetry) {
	clientsMu.Lock()
	defer clientsMu.Unlock()

	for ch := range clients {
		select {
		case ch <- data:
			// Data sent successfully

		default:
			// Client is too slow.
			// Do not block MQTT message handler.
		}
	}
}

// ======================================================
// MQTT MESSAGE HANDLER
// ======================================================

var messageHandler mqtt.MessageHandler = func(
	client mqtt.Client,
	msg mqtt.Message,
) {
	var data Telemetry

	// --------------------------------------------------
	// Parse MQTT JSON
	// --------------------------------------------------

	err := json.Unmarshal(msg.Payload(), &data)

	if err != nil {
		log.Println("JSON error:", err)
		return
	}

	// --------------------------------------------------
	// 1. Save latest data to RAM
	// --------------------------------------------------

	setLatestData(data)

	// --------------------------------------------------
	// 2. Send realtime data to SSE clients
	// --------------------------------------------------

	broadcastTelemetry(data)

	// --------------------------------------------------
	// 3. Save telemetry to Supabase PostgreSQL
	// --------------------------------------------------

	_, err = db.Exec(`
		INSERT INTO telemetry
			(temperature, humidity, light)
		VALUES
			($1, $2, $3)
	`,
		data.Temperature,
		data.Humidity,
		data.Light,
	)

	if err != nil {
		log.Println("Database INSERT error:", err)
	} else {
		fmt.Println("Saved to Supabase PostgreSQL!")
	}

	// --------------------------------------------------
	// 4. Print MQTT data
	// --------------------------------------------------

	fmt.Println("----- MQTT DATA -----")

	fmt.Printf(
		"Temperature: %.2f °C\n",
		data.Temperature,
	)

	fmt.Printf(
		"Humidity:    %.2f %%\n",
		data.Humidity,
	)

	fmt.Printf(
		"Light:       %.2f lux\n",
		data.Light,
	)

	fmt.Println("---------------------")
}

// ======================================================
// SESSION
// ======================================================

var (
	sessions   = make(map[string]string)
	sessionsMu sync.RWMutex
)

// Generate secure random session token
func generateSessionToken() (string, error) {
	bytes := make([]byte, 32)

	_, err := rand.Read(bytes)
	if err != nil {
		return "", err
	}

	return base64.RawURLEncoding.EncodeToString(bytes), nil
}

// Create session
func createSession(username string) (string, error) {
	token, err := generateSessionToken()

	if err != nil {
		return "", err
	}

	sessionsMu.Lock()
	sessions[token] = username
	sessionsMu.Unlock()

	return token, nil
}

// Get username from session
func getSessionUsername(token string) (string, bool) {
	sessionsMu.RLock()
	defer sessionsMu.RUnlock()

	username, ok := sessions[token]

	return username, ok
}

// Delete session
func deleteSession(token string) {
	sessionsMu.Lock()
	defer sessionsMu.Unlock()

	delete(sessions, token)
}

// ======================================================
// COOKIE CONFIGURATION
// ======================================================

func isProduction() bool {
	return os.Getenv("ENV") == "production"
}

// ======================================================
// AUTH MIDDLEWARE
// ======================================================

func requireAuth() gin.HandlerFunc {
	return func(c *gin.Context) {

		cookie, err := c.Cookie("session")

		if err != nil {
			c.Redirect(
				http.StatusFound,
				"/login",
			)

			c.Abort()
			return
		}

		_, ok := getSessionUsername(cookie)

		if !ok {

			c.SetCookie(
				"session",
				"",
				-1,
				"/",
				"",
				isProduction(),
				true,
			)

			c.Redirect(
				http.StatusFound,
				"/login",
			)

			c.Abort()
			return
		}

		c.Next()
	}
}

// ======================================================
// ENSURE ADMIN USER
// ======================================================

func ensureAdminUser() {

	adminPassword := os.Getenv("ADMIN_PASSWORD")

	if adminPassword == "" {
		log.Fatal("ADMIN_PASSWORD is not set")
	}

	var exists bool

	err := db.QueryRow(`
		SELECT EXISTS(
			SELECT 1
			FROM users
			WHERE username = 'admin'
		)
	`).Scan(&exists)

	if err != nil {
		log.Fatal(
			"Check admin user error:",
			err,
		)
	}

	// Admin already exists
	if exists {

		fmt.Println(
			"Admin user already exists!",
		)

		return
	}

	// --------------------------------------------------
	// Hash admin password
	// --------------------------------------------------

	passwordHash, err := bcrypt.GenerateFromPassword(
		[]byte(adminPassword),
		bcrypt.DefaultCost,
	)

	if err != nil {
		log.Fatal(
			"Password hash error:",
			err,
		)
	}

	// --------------------------------------------------
	// Create admin
	// --------------------------------------------------

	_, err = db.Exec(`
		INSERT INTO users
			(username, password)
		VALUES
			($1, $2)
	`,
		"admin",
		string(passwordHash),
	)

	if err != nil {
		log.Fatal(
			"Create admin user error:",
			err,
		)
	}

	fmt.Println(
		"Admin user created!",
	)
}

// ======================================================
// CORS
// ======================================================

func setupCORS(r *gin.Engine) {

	frontendURL := os.Getenv("FRONTEND_URL")

	// Local development
	if frontendURL == "" {
		frontendURL = "http://localhost:5500"
	}

	r.Use(cors.New(cors.Config{
		AllowOrigins: []string{
			frontendURL,
			"http://localhost:5500",
			"http://127.0.0.1:5500",
		},

		AllowMethods: []string{
			"GET",
			"POST",
			"OPTIONS",
		},

		AllowHeaders: []string{
			"Origin",
			"Content-Type",
			"Accept",
		},

		AllowCredentials: true,

		MaxAge: 12 * time.Hour,
	}))
}

// ======================================================
// WEB DIRECTORY
// ======================================================

func getWebDirectory() string {

	// User can explicitly set WEB_DIR
	webDir := os.Getenv("WEB_DIR")

	if webDir != "" {
		return webDir
	}

	// Try ../web
	parentWeb := filepath.Join("..", "web")

	if _, err := os.Stat(parentWeb); err == nil {
		return parentWeb
	}

	// Try ./web
	currentWeb := filepath.Join(".", "web")

	if _, err := os.Stat(currentWeb); err == nil {
		return currentWeb
	}

	return "../web"
}

// ======================================================
// MAIN
// ======================================================

func main() {

	// ==================================================
	// 0. LOAD .ENV
	// ==================================================

	err := godotenv.Load()

	if err != nil {
		log.Println(
			".env file not found, using system environment variables",
		)
	}

	// ==================================================
	// 1. SUPABASE POSTGRESQL
	// ==================================================

	databaseURL := os.Getenv("DATABASE_URL")

	if databaseURL == "" {
		log.Fatal("DATABASE_URL is not set")
	}

	db, err = sql.Open(
		"postgres",
		databaseURL,
	)

	if err != nil {
		log.Fatal(
			"Database open error:",
			err,
		)
	}

	defer db.Close()

	// --------------------------------------------------
	// Test database connection
	// --------------------------------------------------

	err = db.Ping()

	if err != nil {
		log.Fatal(
			"Database connection error:",
			err,
		)
	}

	fmt.Println(
		"Supabase PostgreSQL connected!",
	)

	// ==================================================
	// 2. ADMIN USER
	// ==================================================

	ensureAdminUser()

	// ==================================================
	// 3. HIVE MQ CLOUD
	// ==================================================

	mqttHost := os.Getenv("MQTT_HOST")
	mqttUsername := os.Getenv("MQTT_USERNAME")
	mqttPassword := os.Getenv("MQTT_PASSWORD")

	if mqttHost == "" {
		log.Fatal("MQTT_HOST is not set")
	}

	if mqttUsername == "" {
		log.Fatal("MQTT_USERNAME is not set")
	}

	if mqttPassword == "" {
		log.Fatal("MQTT_PASSWORD is not set")
	}

	// --------------------------------------------------
	// MQTT options
	// --------------------------------------------------

	opts := mqtt.NewClientOptions()

	opts.AddBroker(
		"ssl://" + mqttHost + ":8883",
	)

	opts.SetUsername(
		mqttUsername,
	)

	opts.SetPassword(
		mqttPassword,
	)

	opts.SetClientID(
		"go-iot-backend",
	)

	// --------------------------------------------------
	// TLS
	// --------------------------------------------------

	opts.SetTLSConfig(
		&tls.Config{
			MinVersion: tls.VersionTLS12,
			ServerName: mqttHost,
		},
	)

	// --------------------------------------------------
	// MQTT connected callback
	// --------------------------------------------------

	opts.OnConnect = func(
		client mqtt.Client,
	) {

		fmt.Println(
			"MQTT HiveMQ Cloud connected!",
		)

		token := client.Subscribe(
			"iot/sensors",
			0,
			messageHandler,
		)

		token.Wait()

		if token.Error() != nil {

			log.Println(
				"Subscribe error:",
				token.Error(),
			)

			return
		}

		fmt.Println(
			"Subscribed to: iot/sensors",
		)
	}

	// --------------------------------------------------
	// MQTT connection lost
	// --------------------------------------------------

	opts.OnConnectionLost = func(
		client mqtt.Client,
		err error,
	) {

		log.Println(
			"MQTT connection lost:",
			err,
		)
	}

	// ==================================================
	// Create MQTT client
	// ==================================================

	client := mqtt.NewClient(opts)

	token := client.Connect()

	token.Wait()

	if token.Error() != nil {

		log.Fatal(
			"MQTT connection error:",
			token.Error(),
		)
	}

	// ==================================================
	// 4. GIN WEB SERVER
	// ==================================================

	r := gin.Default()

	// ==================================================
	// 5. CORS
	// ==================================================

	setupCORS(r)

	// ==================================================
	// 6. WEB DIRECTORY
	// ==================================================

	webDir := getWebDirectory()

	fmt.Println(
		"Web directory:",
		webDir,
	)

	// ==================================================
	// 7. LOGIN PAGE
	// ==================================================

	r.GET(
		"/login",
		func(c *gin.Context) {

			c.File(
				filepath.Join(
					webDir,
					"login.html",
				),
			)
		},
	)

	// ==================================================
	// 8. LOGIN API
	// ==================================================

	r.POST(
		"/api/login",
		func(c *gin.Context) {

			var request LoginRequest

			// --------------------------------------------------
			// Read JSON
			// --------------------------------------------------

			if err := c.ShouldBindJSON(
				&request,
			); err != nil {

				c.JSON(
					http.StatusBadRequest,
					gin.H{
						"error": "Invalid request",
					},
				)

				return
			}

			// --------------------------------------------------
			// Validate input
			// --------------------------------------------------

			if request.Username == "" ||
				request.Password == "" {

				c.JSON(
					http.StatusBadRequest,
					gin.H{
						"error": "Username and password are required",
					},
				)

				return
			}

			// --------------------------------------------------
			// Get password hash
			// --------------------------------------------------

			var passwordHash string

			err := db.QueryRow(`
				SELECT password
				FROM users
				WHERE username = $1
			`,
				request.Username,
			).Scan(
				&passwordHash,
			)

			if err != nil {

				if err == sql.ErrNoRows {

					c.JSON(
						http.StatusUnauthorized,
						gin.H{
							"error": "Invalid username or password",
						},
					)

					return
				}

				log.Println(
					"Database login error:",
					err,
				)

				c.JSON(
					http.StatusInternalServerError,
					gin.H{
						"error": "Database error",
					},
				)

				return
			}

			// --------------------------------------------------
			// Compare password
			// --------------------------------------------------

			err = bcrypt.CompareHashAndPassword(
				[]byte(passwordHash),
				[]byte(request.Password),
			)

			if err != nil {

				c.JSON(
					http.StatusUnauthorized,
					gin.H{
						"error": "Invalid username or password",
					},
				)

				return
			}

			// --------------------------------------------------
			// Create session
			// --------------------------------------------------

			sessionToken, err := createSession(
				request.Username,
			)

			if err != nil {

				c.JSON(
					http.StatusInternalServerError,
					gin.H{
						"error": "Cannot create session",
					},
				)

				return
			}

			// --------------------------------------------------
			// Set session cookie
			// --------------------------------------------------

			c.SetCookie(
				"session",
				sessionToken,
				60*60*8,
				"/",
				"",
				isProduction(),
				true,
			)

			c.JSON(
				http.StatusOK,
				gin.H{
					"message": "Login successful",
				},
			)
		},
	)

	// ==================================================
	// 9. LOGOUT API
	// ==================================================

	r.POST(
		"/api/logout",
		func(c *gin.Context) {

			cookie, err := c.Cookie(
				"session",
			)

			if err == nil {
				deleteSession(cookie)
			}

			c.SetCookie(
				"session",
				"",
				-1,
				"/",
				"",
				isProduction(),
				true,
			)

			c.JSON(
				http.StatusOK,
				gin.H{
					"message": "Logged out",
				},
			)
		},
	)

	// ==================================================
	// 10. PROTECTED ROUTES
	// ==================================================

	protected := r.Group("/")

	protected.Use(
		requireAuth(),
	)

	// ==================================================
	// 11. WEB FILES
	// ==================================================

	protected.StaticFile(
		"/style.css",
		filepath.Join(
			webDir,
			"style.css",
		),
	)

	protected.StaticFile(
		"/app.js",
		filepath.Join(
			webDir,
			"app.js",
		),
	)

	// ==================================================
	// 12. DASHBOARD
	// ==================================================

	protected.GET(
		"/",
		func(c *gin.Context) {

			c.File(
				filepath.Join(
					webDir,
					"index.html",
				),
			)
		},
	)

	// ==================================================
	// 13. SSE TELEMETRY
	// ==================================================

	protected.GET(
		"/api/telemetry",
		func(c *gin.Context) {

			// --------------------------------------------------
			// SSE headers
			// --------------------------------------------------

			c.Header(
				"Content-Type",
				"text/event-stream",
			)

			c.Header(
				"Cache-Control",
				"no-cache",
			)

			c.Header(
				"Connection",
				"keep-alive",
			)

			c.Header(
				"X-Accel-Buffering",
				"no",
			)

			// --------------------------------------------------
			// Create SSE client channel
			// --------------------------------------------------

			ch := make(
				chan Telemetry,
				1,
			)

			clientsMu.Lock()

			clients[ch] = struct{}{}

			clientsMu.Unlock()

			// --------------------------------------------------
			// Remove client when disconnected
			// --------------------------------------------------

			defer func() {

				clientsMu.Lock()

				delete(
					clients,
					ch,
				)

				clientsMu.Unlock()

			}()

			// --------------------------------------------------
			// Send latest RAM data immediately
			// --------------------------------------------------

			if data, ok := getLatestData(); ok {

				payload, err := json.Marshal(
					data,
				)

				if err == nil {

					fmt.Fprintf(
						c.Writer,
						"data: %s\n\n",
						payload,
					)

					c.Writer.Flush()
				}
			}

			// --------------------------------------------------
			// SSE loop
			// --------------------------------------------------

			heartbeat := time.NewTicker(
				25 * time.Second,
			)

			defer heartbeat.Stop()

			for {

				select {

				// Browser disconnected
				case <-c.Request.Context().Done():

					return

				// New MQTT telemetry
				case data := <-ch:

					payload, err := json.Marshal(
						data,
					)

					if err != nil {
						continue
					}

					fmt.Fprintf(
						c.Writer,
						"data: %s\n\n",
						payload,
					)

					c.Writer.Flush()

				// Keep SSE connection alive
				case <-heartbeat.C:

					fmt.Fprint(
						c.Writer,
						": heartbeat\n\n",
					)

					c.Writer.Flush()
				}
			}
		},
	)

	// ==================================================
	// 14. START SERVER
	// ==================================================

	port := os.Getenv("PORT")

	if port == "" {
		port = "8080"
	}

	// Make sure PORT is valid
	if _, err := strconv.Atoi(port); err != nil {
		log.Fatal(
			"Invalid PORT:",
			port,
		)
	}

	fmt.Println(
		"HTTP server running on port:",
		port,
	)

	err = r.Run(
		":" + port,
	)

	if err != nil {
		log.Fatal(err)
	}
}
