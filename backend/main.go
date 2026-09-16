package main

import (
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
	_ "github.com/lib/pq"
)

type Telemetry struct {
	Temperature float64 `json:"temperature"`
	Humidity    float64 `json:"humidity"`
	Light       float64 `json:"light"`
}

type LoginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

var db *sql.DB

var (
	latestData    Telemetry
	hasLatestData bool
	dataMu        sync.RWMutex
)

func getLatestData() (Telemetry, bool) {
	dataMu.RLock()
	defer dataMu.RUnlock()

	return latestData, hasLatestData
}

func setLatestData(data Telemetry) {
	dataMu.Lock()
	latestData = data
	hasLatestData = true
	dataMu.Unlock()
}

var (
	clients   = make(map[chan Telemetry]struct{})
	clientsMu sync.Mutex
)

func broadcastTelemetry(data Telemetry) {
	clientsMu.Lock()
	defer clientsMu.Unlock()

	for ch := range clients {
		select {
		case ch <- data:
		default:
		}
	}
}

var messageHandler mqtt.MessageHandler = func(
	client mqtt.Client,
	msg mqtt.Message,
) {
	var data Telemetry

	err := json.Unmarshal(msg.Payload(), &data)
	if err != nil {
		log.Println("JSON error:", err)
		return
	}

	setLatestData(data)
	broadcastTelemetry(data)

	_, err = db.Exec(`
		INSERT INTO telemetry (temperature, humidity, light)
		VALUES ($1, $2, $3)
	`,
		data.Temperature,
		data.Humidity,
		data.Light,
	)

	if err != nil {
		log.Println("Database INSERT error:", err)
	} else {
		fmt.Println("Saved to PostgreSQL!")
	}

	fmt.Println("----- MQTT DATA -----")
	fmt.Printf("Temperature: %.2f °C\n", data.Temperature)
	fmt.Printf("Humidity:    %.2f %%\n", data.Humidity)
	fmt.Printf("Light:       %.2f lux\n", data.Light)
	fmt.Println("---------------------")
}

//
// SESSION
//

var (
	sessions   = make(map[string]string)
	sessionsMu sync.RWMutex
)

func generateSessionToken() (string, error) {
	bytes := make([]byte, 32)

	_, err := rand.Read(bytes)
	if err != nil {
		return "", err
	}

	return base64.RawURLEncoding.EncodeToString(bytes), nil
}

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

func getSessionUsername(token string) (string, bool) {
	sessionsMu.RLock()
	defer sessionsMu.RUnlock()

	username, ok := sessions[token]

	return username, ok
}

func deleteSession(token string) {
	sessionsMu.Lock()
	delete(sessions, token)
	sessionsMu.Unlock()
}

//
// AUTH MIDDLEWARE
//

func requireAuth() gin.HandlerFunc {
	return func(c *gin.Context) {

		cookie, err := c.Cookie("session")

		if err != nil {
			c.Redirect(http.StatusFound, "/login")

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
				false,
				true,
			)

			c.Redirect(http.StatusFound, "/login")

			c.Abort()

			return
		}

		c.Next()
	}
}

//
// ENSURE ADMIN USER
//

func ensureAdminUser() {

	adminPassword := os.Getenv("ADMIN_PASSWORD")

	if adminPassword == "" {
		adminPassword = "12345678"
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
		log.Fatal("Check admin user error:", err)
	}

	if exists {
		fmt.Println("Admin user already exists!")

		return
	}

	passwordHash, err := bcrypt.GenerateFromPassword(
		[]byte(adminPassword),
		bcrypt.DefaultCost,
	)

	if err != nil {
		log.Fatal("Password hash error:", err)
	}

	_, err = db.Exec(`
		INSERT INTO users (username, password)
		VALUES ($1, $2)
	`,
		"admin",
		string(passwordHash),
	)

	if err != nil {
		log.Fatal("Create admin user error:", err)
	}

	fmt.Println("Admin user created!")
}

func main() {

	//
	// PostgreSQL
	//

	pgUser := os.Getenv("PG_USER")

	if pgUser == "" {
		pgUser = "postgres"
	}

	pgPassword := os.Getenv("PG_PASSWORD")

	connStr := fmt.Sprintf(
		"host=localhost port=5432 user=%s password=%s dbname=iot_db sslmode=disable",
		pgUser,
		pgPassword,
	)

	var err error

	db, err = sql.Open("postgres", connStr)

	if err != nil {
		log.Fatal("Database open error:", err)
	}

	err = db.Ping()

	if err != nil {
		log.Fatal("Database connection error:", err)
	}

	fmt.Println("PostgreSQL connected!")

	//
	// Admin
	//

	ensureAdminUser()

	//
	// MQTT
	//

	opts := mqtt.NewClientOptions()

	opts.AddBroker("tcp://localhost:1883")

	opts.SetClientID("go-iot-backend")

	opts.OnConnect = func(client mqtt.Client) {

		fmt.Println("MQTT connected!")

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

	opts.OnConnectionLost = func(
		client mqtt.Client,
		err error,
	) {
		log.Println(
			"MQTT connection lost:",
			err,
		)
	}

	client := mqtt.NewClient(opts)

	token := client.Connect()

	token.Wait()

	if token.Error() != nil {
		log.Fatal(
			"MQTT connection error:",
			token.Error(),
		)
	}

	//
	// GIN
	//

	r := gin.Default()

	//
	// Login page
	//

	r.GET("/login", func(c *gin.Context) {

		c.File("../../web/login.html")
	})

	//
	// Login API
	//

	r.POST("/api/login", func(c *gin.Context) {

		var request LoginRequest

		if err := c.ShouldBindJSON(&request); err != nil {

			c.JSON(
				http.StatusBadRequest,
				gin.H{
					"error": "Invalid request",
				},
			)

			return
		}

		var passwordHash string

		err := db.QueryRow(`
			SELECT password
			FROM users
			WHERE username = $1
		`,
			request.Username,
		).Scan(&passwordHash)

		if err != nil {

			c.JSON(
				http.StatusUnauthorized,
				gin.H{
					"error": "Invalid username or password",
				},
			)

			return
		}

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

		c.SetCookie(
			"session",
			sessionToken,
			60*60*8,
			"/",
			"",
			false,
			true,
		)

		c.JSON(
			http.StatusOK,
			gin.H{
				"message": "Login successful",
			},
		)
	})

	//
	// Logout
	//

	r.POST("/api/logout", func(c *gin.Context) {

		cookie, err := c.Cookie("session")

		if err == nil {
			deleteSession(cookie)
		}

		c.SetCookie(
			"session",
			"",
			-1,
			"/",
			"",
			false,
			true,
		)

		c.JSON(
			http.StatusOK,
			gin.H{
				"message": "Logged out",
			},
		)
	})

	//
	// Protected routes
	//

	protected := r.Group("/")

	protected.Use(requireAuth())

	{
		protected.StaticFile(
			"/style.css",
			"../../web/style.css",
		)

		protected.StaticFile(
			"/app.js",
			"../../web/app.js",
		)

		protected.GET("/", func(c *gin.Context) {

			c.File("../../web/index.html")
		})

		protected.GET(
			"/api/telemetry",
			func(c *gin.Context) {

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

				ch := make(chan Telemetry, 1)

				clientsMu.Lock()

				clients[ch] = struct{}{}

				clientsMu.Unlock()

				defer func() {

					clientsMu.Lock()

					delete(clients, ch)

					close(ch)

					clientsMu.Unlock()
				}()

				if data, ok := getLatestData(); ok {

					payload, err := json.Marshal(data)

					if err == nil {

						fmt.Fprintf(
							c.Writer,
							"data: %s\n\n",
							payload,
						)

						c.Writer.Flush()
					}
				}

				for {

					select {

					case <-c.Request.Context().Done():

						return

					case data := <-ch:

						payload, err := json.Marshal(data)

						if err != nil {
							continue
						}

						fmt.Fprintf(
							c.Writer,
							"data: %s\n\n",
							payload,
						)

						c.Writer.Flush()
					}
				}
			},
		)
	}

	//
	// Server
	//

	fmt.Println(
		"HTTP server running on http://localhost:8080",
	)

	err = r.Run(":8080")

	if err != nil {
		log.Fatal(err)
	}
}