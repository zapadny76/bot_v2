package main

import (
	"context"
	"encoding/csv"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"botv2/config"
	"botv2/models"
	"botv2/utils"

	"github.com/gin-gonic/gin"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/joho/godotenv"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readpref"
)

// Функция для инициализации карты состояний
func initUserStates(app *models.AppContext) {
	app.UserStates = make(map[int64]string)
	app.UserStateData = make(map[int64]interface{})
	// Добавляем хранилище для временных данных
	app.UserStatesMutex = &sync.Mutex{}
}

// Установка состояния пользователя
func setUserState(app *models.AppContext, chatID int64, state string) {
	app.UserStatesMutex.Lock()
	defer app.UserStatesMutex.Unlock()
	app.UserStates[chatID] = state

	// Можно также логировать изменение состояния
	log.Printf("Изменение состояния пользователя %d: %s", chatID, state)
}

// Получение текущего состояния пользователя
func getUserState(app *models.AppContext, chatID int64) string {
	app.UserStatesMutex.Lock()
	defer app.UserStatesMutex.Unlock()

	state, exists := app.UserStates[chatID]
	if !exists {
		// Если состояние не найдено, проверяем, зарегистрирован ли пользователь
		isRegistered, err := isUserRegistered(app, chatID)
		if err != nil {
			log.Printf("Ошибка проверки регистрации: %v", err)
			return config.STATE_NEW
		}

		if isRegistered {
			// Устанавливаем состояние для зарегистрированного пользователя
			app.UserStates[chatID] = config.STATE_WAITING_READINGS
			return config.STATE_WAITING_READINGS
		} else {
			// Устанавливаем состояние для нового пользователя
			app.UserStates[chatID] = config.STATE_NEW
			return config.STATE_NEW
		}
	}

	return state
}

// Удаление состояния пользователя (если нужно)
func clearUserState(app *models.AppContext, chatID int64) {
	app.UserStatesMutex.Lock()
	defer app.UserStatesMutex.Unlock()
	delete(app.UserStates, chatID)
}

// Инициализация приложения
func initApp() (*models.AppContext, error) {
	// Загрузка переменных окружения
	if err := godotenv.Load(".env"); err != nil {
		log.Printf("Предупреждение: не удалось загрузить .env файл: %v", err)
	}
	// Получение токена и URI MongoDB из переменных окружения
	token := os.Getenv("TELEGRAM_TOKEN")
	mongoURI := os.Getenv("MONGO_URI")

	if token == "" || mongoURI == "" {
		return nil, fmt.Errorf("не указан TELEGRAM_TOKEN или MONGO_URI")
	}

	// Инициализация папок для логов и бэкапов
	for _, dir := range []string{config.LOG_FOLDER, config.BACKUP_FOLDER} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("не удалось создать директорию %s: %v", dir, err)
		}
	}

	// Настройка журнала
	logFile, err := os.OpenFile(
		filepath.Join(config.LOG_FOLDER, fmt.Sprintf("bot_%s.log", time.Now().Format("2006-01-02"))),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND,
		0o666,
	)
	if err != nil {
		return nil, fmt.Errorf("не удалось открыть файл лога: %v", err)
	}
	log.SetOutput(logFile)

	// Инициализация Telegram бота
	bot, err := tgbotapi.NewBotAPI(token)
	if err != nil {
		return nil, fmt.Errorf("ошибка при создании бота: %v", err)
	}

	// Инициализация MongoDB с таймаутом
	ctx, cancel := context.WithTimeout(context.Background(), config.DATABASE_TIMEOUT)
	defer cancel()

	clientOptions := options.Client().
		ApplyURI(mongoURI).
		SetConnectTimeout(config.DATABASE_TIMEOUT).
		SetServerSelectionTimeout(config.DATABASE_TIMEOUT)

	client, err := mongo.Connect(ctx, clientOptions)
	if err != nil {
		return nil, fmt.Errorf("ошибка подключения к MongoDB: %v", err)
	}

	// Проверка соединения с MongoDB
	if err := client.Ping(ctx, readpref.Primary()); err != nil {
		return nil, fmt.Errorf("не удалось подключиться к MongoDB: %v", err)
	}
	// Инициализация коллекций
	database := client.Database("water_meters")
	usersCollection := database.Collection("users")
	readingsCollection := database.Collection("readings")
	newsCollection := database.Collection("news")
	adminsCollection := database.Collection("admins")
	configCollection := database.Collection("config")
	apartmentsCollection := database.Collection("apartments")
	passportCollection := database.Collection("passports")
	// Добавляем коллекцию для аварийных заявок
	emergencyCollection := database.Collection("emergency_requests")
	// Создание индексов для оптимизации запросов
	createIndexes(ctx, usersCollection, readingsCollection, newsCollection, adminsCollection)
	// Проверяем наличие квартир
	// Проверка наличия квартир
	count, err := apartmentsCollection.CountDocuments(ctx, bson.M{})
	if err == nil && count == 0 {
		var apartments []interface{}
		for i := config.MinApartmentNumber; i <= config.MaxApartmentNumber; i++ {
			apartments = append(apartments, models.Apartment{
				Number:     i,
				TotalArea:  0,
				LivingArea: 0,
				Residents:  []int64{},
			})
		}
		_, err = apartmentsCollection.InsertMany(ctx, apartments)
		if err != nil {
			log.Printf("Ошибка инициализации квартир: %v", err)
		}
	}

	// Загрузка конфигурации
	var config models.Config
	err = configCollection.FindOne(ctx, bson.M{}).Decode(&config)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			// Создание конфигурации по умолчанию, если она не существует
			config = models.Config{
				AdminIDs:             []int64{},
				ReadingReminderDay:   25,
				AllowReadingsFrom:    1,
				AllowReadingsTo:      31,
				MaxColdWaterIncrease: config.MaxColdWaterIncrease,
				MaxHotWaterIncrease:  config.MaxHotWaterIncrease,
				AutoConfirmReadings:  true,
				BackupIntervalHours:  24,
				MaintenanceMode:      false,
				MaintenanceMessage:   "Бот находится на техническом обслуживании. Пожалуйста, попробуйте позже.",
			}

			_, err = configCollection.InsertOne(ctx, config)
			if err != nil {
				return nil, fmt.Errorf("ошибка при создании конфигурации: %v", err)
			}
		} else {
			return nil, fmt.Errorf("ошибка при загрузке конфигурации: %v", err)
		}
	}

	app := &models.AppContext{
		Bot:                  bot,
		MongoClient:          client,
		UsersCollection:      usersCollection,
		ReadingsCollection:   readingsCollection,
		NewsCollection:       newsCollection,
		AdminsCollection:     adminsCollection,
		ConfigCollection:     configCollection,
		ApartmentsCollection: apartmentsCollection,
		PassportCollection:   passportCollection,
		EmergencyCollection:  emergencyCollection, // Добавляем в AppContext
		Config:               config,
	}
	// Инициализация карты состояний
	initUserStates(app)
	// Инициализация Gin
	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	router.Use(gin.Recovery())

	app.GinRouter = router

	return app, nil
}

func setupAPI(app *models.AppContext) {
	router := app.GinRouter
	router.Use(authMiddleware(app))
	// Добавьте логирование для отладки
	router.Use(func(c *gin.Context) {
		log.Printf("Запрос: %s %s", c.Request.Method, c.Request.URL.Path)
		c.Next()
	})
	api := router.Group("/api/v1")
	{
		api.GET("/stats/:apartment", getStatsHandler(app))
		api.GET("/users", listUsersHandler(app))
		api.GET("/readings/:apartment", getReadingsHandler(app))
		api.POST("/send-news", postNewsHandler(app))
		api.GET("/latest-readings", getLatestReadingsHandler(app))
		// Endpoints операции с квартирами
		api.GET("/apartments", listApartments(app))
		api.GET("/apartments/:number", getApartment(app))
		api.PUT("/apartments/:number", updateApartment(app))
		api.GET("/apartments/:number/residents", getResidents(app))
		api.GET("/emergency/:apartment", getEmergencyRequestsHandler(app))
		api.GET("/emergency/active", getActiveEmergencyRequestsHandler(app))
		api.POST("/emergency", submitEmergencyRequestHandler(app))
		api.PUT("/emergency/status", updateEmergencyStatusHandler(app))
		// Endpoints для показаний счетчиков
		api.POST("/readings", submitReadingsHandler(app))
		api.POST("/readings/confirm", confirmReadingsHandler(app))
		api.POST("/admin/readings", adminSubmitReadingsHandler(app))
	}

	router.GET("/health", func(c *gin.Context) {
		c.JSON(200, gin.H{"status": "ok"})
	})
}

// Обработчики
//
// Обработчик для получения аварийных заявок квартиры
func getEmergencyRequestsHandler(app *models.AppContext) gin.HandlerFunc {
	return func(c *gin.Context) {
		apartmentStr := c.Param("apartment")
		apartment, err := strconv.Atoi(apartmentStr)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Некорректный номер квартиры"})
			return
		}

		ctx, cancel := context.WithTimeout(context.Background(), config.DATABASE_TIMEOUT)
		defer cancel()

		filter := bson.M{"apartment_number": apartment}
		opts := options.Find().SetSort(bson.M{"date": -1})

		cursor, err := app.EmergencyCollection.Find(ctx, filter, opts)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Ошибка базы данных: " + err.Error()})
			return
		}
		defer cursor.Close(ctx)

		var requests []models.EmergencyRequest
		if err = cursor.All(ctx, &requests); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Ошибка чтения данных: " + err.Error()})
			return
		}

		c.JSON(http.StatusOK, requests)
	}
}

// Обработчик для получения всех активных аварийных заявок
func getActiveEmergencyRequestsHandler(app *models.AppContext) gin.HandlerFunc {
	return func(c *gin.Context) {
		requests, err := getActiveEmergencyRequests(app)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Ошибка получения заявок: " + err.Error()})
			return
		}

		c.JSON(http.StatusOK, requests)
	}
}

// Обработчик для создания аварийной заявки через API
func submitEmergencyRequestHandler(app *models.AppContext) gin.HandlerFunc {
	return func(c *gin.Context) {
		var request models.EmergencyRequestSubmission
		if err := c.ShouldBindJSON(&request); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Неверный формат запроса: " + err.Error()})
			return
		}

		// Проверка валидности данных
		if request.ApartmentNumber < config.MIN_APARTMENT_NUMBER || request.ApartmentNumber > config.MAX_APARTMENT_NUMBER {
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("Некорректный номер квартиры. Допустимый диапазон: %d-%d", config.MIN_APARTMENT_NUMBER, config.MAX_APARTMENT_NUMBER)})
			return
		}

		if len(request.Description) < 10 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Описание слишком короткое (минимум 10 символов)"})
			return
		}

		// Получаем пользователя для квартиры
		ctx, cancel := context.WithTimeout(context.Background(), config.DATABASE_TIMEOUT)
		defer cancel()

		var user models.User
		err := app.UsersCollection.FindOne(ctx, bson.M{"apartment_number": request.ApartmentNumber}).Decode(&user)
		if err != nil {
			if err == mongo.ErrNoDocuments {
				c.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("Квартира %d не зарегистрирована", request.ApartmentNumber)})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Ошибка базы данных: " + err.Error()})
			return
		}

		// Устанавливаем стандартный приоритет, если не указан
		priority := request.Priority
		if priority <= 0 || priority > 5 {
			priority = 3
		}

		// Создаем заявку
		id, err := createEmergencyRequest(app, request.ApartmentNumber, user.ChatID, request.Description, request.ContactPhone, request.Photos, priority)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Ошибка создания заявки: " + err.Error()})
			return
		}

		c.JSON(http.StatusOK, gin.H{
			"success":   true,
			"id":        id.Hex(),
			"apartment": request.ApartmentNumber,
			"priority":  priority,
			"date":      time.Now().Format(time.RFC3339),
		})
	}
}

// Обработчик для обновления статуса аварийной заявки через API
func updateEmergencyStatusHandler(app *models.AppContext) gin.HandlerFunc {
	return func(c *gin.Context) {
		var request models.EmergencyRequestStatusUpdate
		if err := c.ShouldBindJSON(&request); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Неверный формат запроса: " + err.Error()})
			return
		}

		// Проверка валидности статуса
		validStatuses := []string{
			config.EMERGENCY_STATUS_CONFIRMED,
			config.EMERGENCY_STATUS_IN_PROGRESS,
			config.EMERGENCY_STATUS_COMPLETED,
			config.EMERGENCY_STATUS_REJECTED,
		}

		statusValid := false
		for _, s := range validStatuses {
			if request.Status == s {
				statusValid = true
				break
			}
		}

		if !statusValid {
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("Некорректный статус: %s. Допустимые значения: %v", request.Status, validStatuses)})
			return
		}

		// Обновляем статус
		err := updateEmergencyStatus(app, request.ID, request.Status, request.AssignedTo, request.ResolutionNotes)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Ошибка обновления статуса: " + err.Error()})
			return
		}

		c.JSON(http.StatusOK, gin.H{
			"success":    true,
			"id":         request.ID.Hex(),
			"status":     request.Status,
			"updated_at": time.Now().Format(time.RFC3339),
		})
	}
}

// Функция для получения списка квартир
func listApartments(app *models.AppContext) gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx, cancel := context.WithTimeout(context.Background(), config.DATABASE_TIMEOUT)
		defer cancel()

		cursor, err := app.ApartmentsCollection.Find(ctx, bson.M{})
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		var apartments []models.Apartment
		if err = cursor.All(ctx, &apartments); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		c.JSON(http.StatusOK, apartments)
	}
}

func getApartment(app *models.AppContext) gin.HandlerFunc {
	return func(c *gin.Context) {
		number, err := strconv.Atoi(c.Param("number"))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid apartment number"})
			return
		}

		ctx, cancel := context.WithTimeout(context.Background(), config.DATABASE_TIMEOUT)
		defer cancel()

		var apartment models.Apartment
		err = app.ApartmentsCollection.FindOne(
			ctx,
			bson.M{"number": number},
		).Decode(&apartment)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "Apartment not found"})
			return
		}

		c.JSON(http.StatusOK, apartment)
	}
}

func updateApartment(app *models.AppContext) gin.HandlerFunc {
	return func(c *gin.Context) {
		number, err := strconv.Atoi(c.Param("number"))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid apartment number"})
			return
		}

		var update struct {
			TotalArea  float64 `json:"total_area"`
			LivingArea float64 `json:"living_area"`
		}
		if err := c.BindJSON(&update); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		ctx, cancel := context.WithTimeout(context.Background(), config.DATABASE_TIMEOUT)
		defer cancel()

		_, err = app.ApartmentsCollection.UpdateOne(
			ctx,
			bson.M{"number": number},
			bson.M{"$set": bson.M{
				"total_area":  update.TotalArea,
				"living_area": update.LivingArea,
			}},
		)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		c.JSON(http.StatusOK, gin.H{"status": "updated"})
	}
}

// GetResidents returns a handler function that retrieves residents of an apartment.
func getResidents(app *models.AppContext) gin.HandlerFunc {
	return func(c *gin.Context) {
		// Получение номера квартиры из параметров запроса
		number, err := strconv.Atoi(c.Param("number"))
		if err != nil || number < config.MIN_APARTMENT_NUMBER || number > config.MAX_APARTMENT_NUMBER {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid apartment number"})
			return
		}

		// Создание контекста с таймаутом
		ctx, cancel := context.WithTimeout(context.Background(), config.DATABASE_TIMEOUT)
		defer cancel()

		// Поиск квартиры в коллекции Apartments
		var apartment models.Apartment
		err = app.ApartmentsCollection.FindOne(
			ctx,
			bson.M{"number": number},
		).Decode(&apartment)
		if err != nil {
			if err == mongo.ErrNoDocuments {
				c.JSON(http.StatusNotFound, gin.H{"error": "Apartment not found"})
			} else {
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			}
			return
		}

		// Если список жильцов пуст, возвращаем пустой массив
		if len(apartment.Residents) == 0 {
			c.JSON(http.StatusOK, []models.User{})
			return
		}

		// Получение данных пользователей из коллекции Users
		var users []models.User
		cursor, err := app.UsersCollection.Find(
			ctx,
			bson.M{"chat_id": bson.M{"$in": apartment.Residents}},
		)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		if err = cursor.All(ctx, &users); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		// Возврат данных пользователей
		c.JSON(http.StatusOK, users)
	}
}

// Middleware для аутентификации
func authMiddleware(app *models.AppContext) gin.HandlerFunc {
	return func(c *gin.Context) {
		apiKey := c.GetHeader("X-API-Key")
		log.Printf("Attempting authentication with key: %s", apiKey)

		// Проверяем ключ в конфигурации
		isValid := false
		for _, key := range app.Config.APIKeys {
			if apiKey == key {
				isValid = true
				break
			}
		}

		if !isValid {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "Invalid API key"})
			return
		}

		c.Next()
	}
}

// Обработчик для получения статистики
func getStatsHandler(app *models.AppContext) gin.HandlerFunc {
	return func(c *gin.Context) {
		apartment, err := strconv.Atoi(c.Param("apartment"))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid apartment number"})
			return
		}

		stats, err := getStatistics(app, apartment)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		c.JSON(http.StatusOK, gin.H{"stats": stats})
	}
}

// Обработчик для получения показаний
func getReadingsHandler(app *models.AppContext) gin.HandlerFunc {
	return func(c *gin.Context) {
		apartment, err := strconv.Atoi(c.Param("apartment"))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid apartment number"})
			return
		}

		limit := 100
		if l := c.Query("limit"); l != "" {
			if limit, err = strconv.Atoi(l); err != nil {
				limit = 100
			}
		}

		ctx, cancel := context.WithTimeout(context.Background(), config.DATABASE_TIMEOUT)
		defer cancel()

		filter := bson.M{"apartment_number": apartment}
		opts := options.Find().SetSort(bson.M{"date": -1}).SetLimit(int64(limit))

		cursor, err := app.ReadingsCollection.Find(ctx, filter, opts)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		var readings []models.Reading
		if err = cursor.All(ctx, &readings); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		c.JSON(http.StatusOK, readings)
	}
}

// Обработчик для отправки новостей
func postNewsHandler(app *models.AppContext) gin.HandlerFunc {
	return func(c *gin.Context) {
		var request struct {
			Title   string `json:"title"`
			Content string `json:"content"`
		}

		if err := c.BindJSON(&request); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request format"})
			return
		}

		if len(request.Title) == 0 || len(request.Content) == 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Title and content are required"})
			return
		}

		// Создаем новость
		news := models.News{
			Title:     request.Title,
			Content:   request.Content,
			Date:      time.Now(),
			CreatedBy: "API",
			Sent:      false,
		}

		ctx, cancel := context.WithTimeout(context.Background(), config.DATABASE_TIMEOUT)
		defer cancel()

		_, err := app.NewsCollection.InsertOne(ctx, news)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		c.JSON(http.StatusOK, gin.H{"status": "News created successfully"})
	}
}

// Обработчик для получения списка пользователей
func listUsersHandler(app *models.AppContext) gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx, cancel := context.WithTimeout(context.Background(), config.DATABASE_TIMEOUT)
		defer cancel()

		cursor, err := app.UsersCollection.Find(ctx, bson.M{})
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		var users []models.User
		if err = cursor.All(ctx, &users); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		c.JSON(http.StatusOK, users)
	}
}

// Запуск сервера
func startHTTPServer(app *models.AppContext) {
	port := os.Getenv("HTTP_PORT")
	if port == "" {
		port = "8080"
	}

	log.Printf("Starting HTTP server on :%s", port)
	if err := app.GinRouter.Run(":" + port); err != nil {
		log.Fatalf("Failed to start HTTP server: %v", err)
	}
}

// Создание индексов для оптимизации запросов
func createIndexes(ctx context.Context, usersCollection, readingsCollection, newsCollection, adminsCollection *mongo.Collection) {
	// Индекс для быстрого поиска пользователей по chat_id
	usersCollection.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "chat_id", Value: 1}},
		Options: options.Index().SetUnique(true),
	})

	// Индекс для быстрого поиска показаний по apartment_number и date
	readingsCollection.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys: bson.D{{Key: "apartment_number", Value: 1}, {Key: "date", Value: -1}},
	})

	// Индекс для новостей
	newsCollection.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys: bson.D{{Key: "date", Value: -1}},
	})

	// Индекс для администраторов
	adminsCollection.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "chat_id", Value: 1}}, // Исправлено: добавлены Key и Value
		Options: options.Index().SetUnique(true),
	})
}

// Проверка на администратора
func isAdmin(app *models.AppContext, chatID int64) bool {
	ctx, cancel := context.WithTimeout(context.Background(), config.DATABASE_TIMEOUT)
	defer cancel()

	// Проверка в базе данных
	var admin models.Admin
	err := app.AdminsCollection.FindOne(ctx, bson.M{"chat_id": chatID}).Decode(&admin)
	if err == nil {
		return true
	}

	// Проверка в конфигурации
	return slices.Contains(app.Config.AdminIDs, chatID)
}

// Обновление времени последней активности пользователя
func updateUserActivity(app *models.AppContext, chatID int64) {
	ctx, cancel := context.WithTimeout(context.Background(), config.DATABASE_TIMEOUT)
	defer cancel()

	_, err := app.UsersCollection.UpdateOne(
		ctx,
		bson.M{"chat_id": chatID},
		bson.M{"$set": bson.M{"last_active": time.Now()}},
	)
	if err != nil {
		log.Printf("Ошибка обновления активности пользователя %d: %v", chatID, err)
	}
}

// Проверка существования пользователя по chat_id
func isUserRegistered(app *models.AppContext, chatID int64) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), config.DATABASE_TIMEOUT)
	defer cancel()

	filter := bson.M{"chat_id": chatID}
	count, err := app.UsersCollection.CountDocuments(ctx, filter)
	if err != nil {
		return false, fmt.Errorf("ошибка проверки регистрации: %v", err)
	}

	return count > 0, nil
}

// Получение данных пользователя
func getUser(app *models.AppContext, chatID int64) (models.User, error) {
	ctx, cancel := context.WithTimeout(context.Background(), config.DATABASE_TIMEOUT)
	defer cancel()

	var user models.User
	err := app.UsersCollection.FindOne(ctx, bson.M{"chat_id": chatID}).Decode(&user)
	if err != nil {
		return models.User{}, fmt.Errorf("ошибка получения данных пользователя: %v", err)
	}

	return user, nil
}

// Регистрация нового пользователя
func registerUser(app *models.AppContext, chatID int64, username string, apartmentNumber int) error {
	ctx, cancel := context.WithTimeout(context.Background(), config.DATABASE_TIMEOUT)
	defer cancel()

	// Проверяем существование квартиры
	var apartment models.Apartment
	err := app.ApartmentsCollection.FindOne(
		ctx,
		bson.M{"number": apartmentNumber},
	).Decode(&apartment)

	if err == mongo.ErrNoDocuments {
		return fmt.Errorf("квартира %d не найдена", apartmentNumber)
	}

	// Создаем пользователя
	user := models.User{
		ChatID:          chatID,
		Username:        username,
		ApartmentNumber: apartmentNumber,
		RegisteredAt:    time.Now(),
		LastActive:      time.Now(),
	}

	// Добавляем пользователя в коллекцию users
	res, err := app.UsersCollection.InsertOne(ctx, user)
	if err != nil {
		return fmt.Errorf("ошибка регистрации: %v", err)
	}

	// Добавляем пользователя в квартиру
	_, err = app.ApartmentsCollection.UpdateOne(
		ctx,
		bson.M{"number": apartmentNumber},
		bson.M{"$addToSet": bson.M{"residents": chatID}},
	)
	if err != nil {
		// Откатываем создание пользователя при ошибке
		app.UsersCollection.DeleteOne(ctx, bson.M{"_id": res.InsertedID})
		return fmt.Errorf("ошибка привязки к квартире: %v", err)
	}
	// Обновляем время последнего измения
	update := bson.M{"$set": bson.M{"UpdateAt": time.Now()}}
	_, err = app.ConfigCollection.UpdateOne(ctx, bson.M{}, update)
	if err != nil {
		return fmt.Errorf("ошибка обновления конфига: %v", err)
	}

	return nil
}

// Сохранение показаний
func saveReadings(app *models.AppContext, apartmentNumber int, chatID int64, cold, hot float64) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), config.DATABASE_TIMEOUT)
	defer cancel()

	// Проверка валидности значений
	if !validateReading(cold) || !validateReading(hot) {
		return "", fmt.Errorf("недопустимые значения показаний")
	}

	// Текущая дата и время приема показаний
	currentTime := time.Now()

	// Создаем новую запись показаний
	reading := models.Reading{
		ApartmentNumber: apartmentNumber,
		ChatID:          chatID,
		Date:            currentTime,
		Cold:            cold,
		Hot:             hot,
		Verified:        app.Config.AutoConfirmReadings,
		UpdatedAt:       time.Now(), // Добавляем поле
	}

	// Вставляем запись в коллекцию readings
	_, err := app.ReadingsCollection.InsertOne(ctx, reading)
	if err != nil {
		return "", fmt.Errorf("ошибка сохранения показаний: %v", err)
	}

	// Форматируем дату для вывода
	formattedTime := currentTime.Format("2006-01-02 15:04")

	// Обновляем время изменений
	update := bson.M{"$set": bson.M{"UpdatedAt": time.Now()}}
	_, err = app.ConfigCollection.UpdateOne(ctx, bson.M{}, update)
	if err != nil {
		return "", fmt.Errorf("ошибка обновления: %v", err)
	}

	// Запись в лог
	log.Printf("Показания сохранены: Квартира=%d, Холодная=%.2f, Горячая=%.2f",
		apartmentNumber, cold, hot)

	return formattedTime, nil
}

// Получение последних показаний для квартиры
func getLastReadings(app *models.AppContext, apartmentNumber int) (float64, float64, time.Time, error) {
	ctx, cancel := context.WithTimeout(context.Background(), config.DATABASE_TIMEOUT)
	defer cancel()

	filter := bson.M{"apartment_number": apartmentNumber}
	sort := options.FindOne().SetSort(bson.M{"date": -1}) // Сортировка по убыванию даты

	var lastReading models.Reading
	err := app.ReadingsCollection.FindOne(ctx, filter, sort).Decode(&lastReading)

	if err == mongo.ErrNoDocuments {
		return 0, 0, time.Time{}, nil
	}

	if err != nil {
		return 0, 0, time.Time{}, fmt.Errorf("ошибка получения последних показаний: %v", err)
	}

	return lastReading.Cold, lastReading.Hot, lastReading.Date, nil
}

// Валидация показаний
func validateReading(value float64) bool {
	return value > 0 && value < config.MAX_WATER_VALUE
}

// Обработка ввода новостей администратором
func handleNewsInput(app *models.AppContext, update tgbotapi.Update) {
	chatID := update.Message.Chat.ID
	messageText := update.Message.Text

	// Проверяем минимальную длину текста
	if len(messageText) < 10 {
		msg := tgbotapi.NewMessage(chatID, "Текст новости слишком короткий. Минимум 10 символов.")
		app.Bot.Send(msg)
		return
	}

	// Разбиваем на заголовок и текст (первая строка - заголовок)
	lines := strings.Split(messageText, "\n")
	title := lines[0]

	// Если текст состоит только из одной строки, используем её и как заголовок и как содержание
	content := messageText
	if len(lines) > 1 {
		content = strings.Join(lines[1:], "\n")
	}

	// Добавляем новость
	err := addNews(app, title, content, update.Message.From.UserName)
	if err != nil {
		log.Printf("Ошибка добавления новости: %v", err)
		msg := tgbotapi.NewMessage(chatID, "Ошибка добавления новости: "+err.Error())
		app.Bot.Send(msg)
		return
	}

	// Возвращаем пользователя в нормальное состояние
	setUserState(app, chatID, config.STATE_WAITING_READINGS)

	// Отправляем подтверждение
	msg := tgbotapi.NewMessage(chatID, "Новость успешно добавлена. Используйте /sendnews для отправки.")
	app.Bot.Send(msg)
}

// Добавление новости
func addNews(app *models.AppContext, title, content, createdBy string) error {
	ctx, cancel := context.WithTimeout(context.Background(), config.DATABASE_TIMEOUT)
	defer cancel()

	// Проверка длины новости
	if len(title) > 100 || len(content) > 1000 {
		return fmt.Errorf("слишком длинное содержание новости")
	}

	news := models.News{
		Title:     title,
		Content:   content,
		Date:      time.Now(),
		CreatedBy: createdBy,
		Sent:      false,
	}

	_, err := app.NewsCollection.InsertOne(ctx, news)
	if err != nil {
		return fmt.Errorf("ошибка добавления новости: %v", err)
	}

	return nil
}

func createEmergencyRequest(app *models.AppContext, apartmentNumber int, chatID int64, description, contactPhone string, photos []string, priority int) (primitive.ObjectID, error) {
	ctx, cancel := context.WithTimeout(context.Background(), config.DATABASE_TIMEOUT)
	defer cancel()

	// Проверка на максимальную длину описания
	if len(description) > 1000 {
		return primitive.NilObjectID, fmt.Errorf("описание слишком длинное (максимум 1000 символов)")
	}

	// Если приоритет не указан или некорректен, устанавливаем стандартный - 3
	if priority <= 0 || priority > 5 {
		priority = 3
	}

	// Создаем заявку
	emergency := models.EmergencyRequest{
		ApartmentNumber: apartmentNumber,
		ChatID:          chatID,
		Description:     description,
		Date:            time.Now(),
		Status:          config.EMERGENCY_STATUS_NEW,
		ContactPhone:    contactPhone,
		Photos:          photos,
		Priority:        priority,
		UpdatedAt:       time.Now(),
	}

	// Вставляем в базу данных
	result, err := app.EmergencyCollection.InsertOne(ctx, emergency)
	if err != nil {
		return primitive.NilObjectID, fmt.Errorf("ошибка сохранения заявки: %v", err)
	}

	id := result.InsertedID.(primitive.ObjectID)

	// Логируем создание заявки
	log.Printf("Создана аварийная заявка ID=%s, Квартира=%d, Приоритет=%d",
		id.Hex(), apartmentNumber, priority)

	// Оповещаем администраторов о новой аварийной заявке
	go notifyAdminsAboutEmergency(app, id, apartmentNumber, description, priority)

	return id, nil
}

// Получение аварийных заявок пользователя
func getUserEmergencyRequests(app *models.AppContext, chatID int64) ([]models.EmergencyRequest, error) {
	ctx, cancel := context.WithTimeout(context.Background(), config.DATABASE_TIMEOUT)
	defer cancel()

	filter := bson.M{"chat_id": chatID}
	opts := options.Find().SetSort(bson.M{"date": -1})

	cursor, err := app.EmergencyCollection.Find(ctx, filter, opts)
	if err != nil {
		return nil, fmt.Errorf("ошибка получения заявок: %v", err)
	}
	defer cursor.Close(ctx)

	var requests []models.EmergencyRequest
	if err = cursor.All(ctx, &requests); err != nil {
		return nil, fmt.Errorf("ошибка чтения заявок: %v", err)
	}

	return requests, nil
}

// Получение всех активных аварийных заявок
func getActiveEmergencyRequests(app *models.AppContext) ([]models.EmergencyRequest, error) {
	ctx, cancel := context.WithTimeout(context.Background(), config.DATABASE_TIMEOUT)
	defer cancel()

	// Считаем активными заявки в статусах NEW, CONFIRMED и IN_PROGRESS
	filter := bson.M{
		"status": bson.M{
			"$in": []string{
				config.EMERGENCY_STATUS_NEW,
				config.EMERGENCY_STATUS_CONFIRMED,
				config.EMERGENCY_STATUS_IN_PROGRESS,
			},
		},
	}
	opts := options.Find().SetSort(bson.M{"priority": -1, "date": 1})

	cursor, err := app.EmergencyCollection.Find(ctx, filter, opts)
	if err != nil {
		return nil, fmt.Errorf("ошибка получения заявок: %v", err)
	}
	defer cursor.Close(ctx)

	var requests []models.EmergencyRequest
	if err = cursor.All(ctx, &requests); err != nil {
		return nil, fmt.Errorf("ошибка чтения заявок: %v", err)
	}

	return requests, nil
}

// Обновление статуса аварийной заявки
func updateEmergencyStatus(app *models.AppContext, id primitive.ObjectID, status, assignedTo, notes string) error {
	ctx, cancel := context.WithTimeout(context.Background(), config.DATABASE_TIMEOUT)
	defer cancel()

	// Проверяем корректность статуса
	validStatuses := []string{
		config.EMERGENCY_STATUS_CONFIRMED,
		config.EMERGENCY_STATUS_IN_PROGRESS,
		config.EMERGENCY_STATUS_COMPLETED,
		config.EMERGENCY_STATUS_REJECTED,
	}

	statusValid := false
	for _, s := range validStatuses {
		if status == s {
			statusValid = true
			break
		}
	}

	if !statusValid {
		return fmt.Errorf("некорректный статус: %s", status)
	}

	// Подготавливаем обновление
	update := bson.M{
		"$set": bson.M{
			"status":     status,
			"updated_at": time.Now(),
		},
	}

	// Добавляем необязательные поля, если они указаны
	if assignedTo != "" {
		update["$set"].(bson.M)["assigned_to"] = assignedTo
	}

	if notes != "" {
		update["$set"].(bson.M)["resolution_notes"] = notes
	}

	// Если статус завершен, добавляем дату завершения
	if status == config.EMERGENCY_STATUS_COMPLETED {
		update["$set"].(bson.M)["completed_date"] = time.Now()
	}

	// Обновляем в базе данных
	result, err := app.EmergencyCollection.UpdateOne(
		ctx,
		bson.M{"_id": id},
		update,
	)
	if err != nil {
		return fmt.Errorf("ошибка обновления статуса: %v", err)
	}

	if result.MatchedCount == 0 {
		return fmt.Errorf("заявка с ID %s не найдена", id.Hex())
	}

	// Получаем обновленную заявку для уведомления пользователя
	var updatedRequest models.EmergencyRequest
	err = app.EmergencyCollection.FindOne(ctx, bson.M{"_id": id}).Decode(&updatedRequest)
	if err != nil {
		log.Printf("Ошибка получения обновленной заявки: %v", err)
		return nil // Не возвращаем ошибку, т.к. обновление уже выполнено успешно
	}

	// Отправляем уведомление пользователю об изменении статуса
	go notifyUserAboutEmergencyUpdate(app, updatedRequest)

	return nil
}

// Уведомление администраторов о новой аварийной заявке
func notifyAdminsAboutEmergency(app *models.AppContext, id primitive.ObjectID, apartmentNumber int, description string, priority int) {
	// Получаем список администраторов
	ctx, cancel := context.WithTimeout(context.Background(), config.DATABASE_TIMEOUT)
	defer cancel()

	var admins []models.Admin
	cursor, err := app.AdminsCollection.Find(ctx, bson.M{})
	if err != nil {
		log.Printf("Ошибка получения списка админов: %v", err)
		return
	}

	if err = cursor.All(ctx, &admins); err != nil {
		log.Printf("Ошибка декодирования админов: %v", err)
		return
	}

	// Формируем сообщение
	message := fmt.Sprintf(
		"🚨 НОВАЯ АВАРИЙНАЯ ЗАЯВКА 🚨\n\n"+
			"ID: %s\n"+
			"Квартира: %d\n"+
			"Приоритет: %d/5\n"+
			"Дата: %s\n\n"+
			"Описание: %s",
		id.Hex(),
		apartmentNumber,
		priority,
		time.Now().Format("02.01.2006 15:04:05"),
		description,
	)

	// Создаем клавиатуру для быстрых действий
	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("✅ Подтвердить", "emergency_confirm_"+id.Hex()),
			tgbotapi.NewInlineKeyboardButtonData("❌ Отклонить", "emergency_reject_"+id.Hex()),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🛠 Взять в работу", "emergency_progress_"+id.Hex()),
		),
	)

	// Отправляем уведомления всем администраторам
	for _, admin := range admins {
		msg := tgbotapi.NewMessage(admin.ChatID, message)
		msg.ReplyMarkup = keyboard
		_, err := app.Bot.Send(msg)
		if err != nil {
			log.Printf("Ошибка отправки уведомления админу %d: %v", admin.ChatID, err)
		}
	}

	// Также отправляем уведомления админам из списка Config.AdminIDs
	for _, adminID := range app.Config.AdminIDs {
		msg := tgbotapi.NewMessage(adminID, message)
		msg.ReplyMarkup = keyboard
		_, err := app.Bot.Send(msg)
		if err != nil {
			log.Printf("Ошибка отправки уведомления админу %d: %v", adminID, err)
		}
	}
}

// Уведомление пользователя об обновлении статуса заявки
func notifyUserAboutEmergencyUpdate(app *models.AppContext, request models.EmergencyRequest) {
	// Формируем сообщение об обновлении
	var statusText string
	switch request.Status {
	case config.EMERGENCY_STATUS_CONFIRMED:
		statusText = "✅ Подтверждена"
	case config.EMERGENCY_STATUS_IN_PROGRESS:
		statusText = "🛠 В работе"
	case config.EMERGENCY_STATUS_COMPLETED:
		statusText = "✓ Выполнена"
	case config.EMERGENCY_STATUS_REJECTED:
		statusText = "❌ Отклонена"
	default:
		statusText = request.Status
	}

	message := fmt.Sprintf(
		"📝 Обновлен статус вашей аварийной заявки\n\n"+
			"ID: %s\n"+
			"Новый статус: %s\n"+
			"Дата обновления: %s",
		request.ID.Hex(),
		statusText,
		time.Now().Format("02.01.2006 15:04:05"),
	)

	// Добавляем информацию об исполнителе и примечания, если есть
	if request.AssignedTo != "" {
		message += fmt.Sprintf("\nНазначена: %s", request.AssignedTo)
	}

	if request.ResolutionNotes != "" {
		message += fmt.Sprintf("\n\nПримечания: %s", request.ResolutionNotes)
	}

	// Отправляем сообщение пользователю
	msg := tgbotapi.NewMessage(request.ChatID, message)
	_, err := app.Bot.Send(msg)
	if err != nil {
		log.Printf("Ошибка отправки уведомления пользователю %d: %v", request.ChatID, err)
	}
}

// Отправка всех новостей пользователям
func sendNewsToUsers(app *models.AppContext) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), config.DATABASE_TIMEOUT)
	defer cancel()

	// Находим все непосланные новости
	filter := bson.M{"sent": false}
	newsCursor, err := app.NewsCollection.Find(ctx, filter, options.Find().SetSort(bson.M{"date": -1}))
	if err != nil {
		return 0, fmt.Errorf("ошибка получения новостей: %v", err)
	}
	defer newsCursor.Close(ctx)

	// Преобразуем новости в сообщения
	var newsMessages []models.News
	if err = newsCursor.All(ctx, &newsMessages); err != nil {
		return 0, fmt.Errorf("ошибка чтения новостей: %v", err)
	}

	if len(newsMessages) == 0 {
		return 0, nil
	}

	// Находим всех зарегистрированных пользователей
	usersCursor, err := app.UsersCollection.Find(ctx, bson.M{})
	if err != nil {
		return 0, fmt.Errorf("ошибка получения списка пользователей: %v", err)
	}
	defer usersCursor.Close(ctx)

	var users []models.User
	if err = usersCursor.All(ctx, &users); err != nil {
		return 0, fmt.Errorf("ошибка чтения пользователей: %v", err)
	}

	sentCount := 0
	// Отправляем новости каждому пользователю
	for _, user := range users {
		for _, news := range newsMessages {
			message := fmt.Sprintf(
				"❗️ Новость (%s):\n%s\nДата: %s",
				news.Title,
				news.Content,
				news.Date.Format("2006-01-02"),
			)

			msg := tgbotapi.NewMessage(user.ChatID, message)
			_, err := app.Bot.Send(msg)
			// Обрабатываем ошибки отправки (пользователь мог заблокировать бота)
			if err != nil {
				log.Printf("Ошибка отправки новости пользователю %d: %v", user.ChatID, err)
				continue
			}
			sentCount++

			// Добавляем небольшую задержку, чтобы не превысить лимиты API Telegram
			time.Sleep(100 * time.Millisecond)
		}
	}

	// Помечаем новости как отправленные
	for _, news := range newsMessages {
		_, err := app.NewsCollection.UpdateOne(
			ctx,
			bson.M{"_id": news.ID},
			bson.M{"$set": bson.M{"sent": true}},
		)
		if err != nil {
			log.Printf("Ошибка обновления статуса новости %s: %v", news.ID, err)
		}
	}

	return sentCount, nil
}

func sendReminders(app *models.AppContext) (int, error) {
	// Создаем контекст с тайм-аутом
	ctx, cancel := context.WithTimeout(context.Background(), config.DATABASE_TIMEOUT)
	defer cancel()

	// Получаем текущую дату и время в UTC
	now := time.Now().UTC()
	// Вычисляем начало текущего месяца
	startOfMonth := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)

	// Создаем пайплайн для MongoDB агрегации
	pipeline := mongo.Pipeline{
		// Этап 1: Находим пользователей с номером квартиры
		bson.D{
			{Key: "$match", Value: bson.D{
				{Key: "apartment_number", Value: bson.M{"$exists": true}},
			}},
		},
		// Этап 2: Присоединяем коллекцию показаний для поиска последних показаний
		bson.D{
			{Key: "$lookup", Value: bson.D{
				{Key: "from", Value: "readings"},
				{Key: "let", Value: bson.D{{Key: "chatId", Value: "$chat_id"}}},
				{Key: "pipeline", Value: mongo.Pipeline{
					// Подэтап 2.1: Ищем показания для данного пользователя с начала месяца
					bson.D{
						{Key: "$match", Value: bson.D{
							{Key: "$expr", Value: bson.D{
								{Key: "$and", Value: bson.A{
									bson.D{{Key: "$eq", Value: bson.A{"$chat_id", "$$chatId"}}},
									bson.D{{Key: "$gte", Value: bson.A{"$date", primitive.NewDateTimeFromTime(startOfMonth)}}},
								}},
							}},
						}},
					},
					// Подэтап 2.2: Сортируем по дате убывания (сначала новые)
					bson.D{{Key: "$sort", Value: bson.D{{Key: "date", Value: -1}}}},
					// Подэтап 2.3: Берем только первую запись
					bson.D{{Key: "$limit", Value: 1}},
				}},
				{Key: "as", Value: "last_reading"},
			}},
		},
		// Этап 3: Оставляем только пользователей, у которых нет показаний за текущий месяц
		bson.D{
			{Key: "$match", Value: bson.D{
				{Key: "last_reading", Value: bson.D{{Key: "$size", Value: 0}}},
			}},
		},
	}

	// Выполняем агрегацию
	cursor, err := app.UsersCollection.Aggregate(ctx, pipeline)
	if err != nil {
		return 0, fmt.Errorf("ошибка агрегации пользователей: %v", err)
	}
	defer cursor.Close(ctx)

	// Получаем результаты
	var users []models.User
	if err = cursor.All(ctx, &users); err != nil {
		return 0, fmt.Errorf("ошибка декодирования пользователей: %v", err)
	}

	// Отправляем напоминания
	sentCount := 0
	for _, user := range users {
		msg := tgbotapi.NewMessage(user.ChatID, "Напоминание: необходимо передать показания счетчиков воды.")
		_, err := app.Bot.Send(msg)
		if err != nil {
			log.Printf("Ошибка отправки напоминания пользователю %d: %v", user.ChatID, err)
			continue
		}
		sentCount++
		// Небольшая задержка, чтобы не превысить лимиты Telegram API
		time.Sleep(100 * time.Millisecond)
	}

	// Возвращаем количество отправленных напоминаний
	return sentCount, nil
}

// Получение истории показаний для квартиры
func getHistory(app *models.AppContext, apartmentNumber int, limit int) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), config.DATABASE_TIMEOUT)
	defer cancel()

	filter := bson.M{"apartment_number": apartmentNumber}
	options := options.Find().
		SetSort(bson.M{"date": -1}).
		SetLimit(int64(limit))

	cursor, err := app.ReadingsCollection.Find(ctx, filter, options)
	if err != nil {
		return "", fmt.Errorf("ошибка получения истории: %v", err)
	}
	defer cursor.Close(ctx)

	var readings []models.Reading
	if err = cursor.All(ctx, &readings); err != nil {
		return "", fmt.Errorf("ошибка чтения показаний: %v", err)
	}

	if len(readings) == 0 {
		return config.MSG_NO_READINGS, nil
	}

	// Формирование ответа
	response := config.MSG_HISTORY_TITLE
	for i, reading := range readings {
		response += fmt.Sprintf(
			config.MSG_HISTORY_ENTRY,
			i+1,
			reading.Cold,
			reading.Hot,
			reading.Date.Format("2006-01-02"),
		)
	}

	return response, nil
}

func getLatestReadingsHandler(app *models.AppContext) gin.HandlerFunc {
	return func(c *gin.Context) {
		now := time.Now().UTC()
		startOfMonth := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
		endOfMonth := startOfMonth.AddDate(0, 1, 0).Add(-time.Nanosecond)

		// Формируем правильный пайплайн агрегации
		pipeline := mongo.Pipeline{
			bson.D{
				{Key: "$lookup", Value: bson.D{
					{Key: "from", Value: "readings"},
					{Key: "let", Value: bson.D{{Key: "userChatID", Value: "$chat_id"}}},
					{Key: "pipeline", Value: mongo.Pipeline{
						bson.D{
							{Key: "$match", Value: bson.D{
								{Key: "$expr", Value: bson.D{
									{Key: "$and", Value: bson.A{
										bson.D{{Key: "$eq", Value: bson.A{"$chat_id", "$$userChatID"}}},
										bson.D{{Key: "$gte", Value: bson.A{"$date", primitive.NewDateTimeFromTime(startOfMonth)}}},
										bson.D{{Key: "$lte", Value: bson.A{"$date", primitive.NewDateTimeFromTime(endOfMonth)}}},
									}},
								}},
							}},
						},
						bson.D{{Key: "$sort", Value: bson.D{{Key: "date", Value: -1}}}},
						bson.D{{Key: "$limit", Value: 1}},
					}},
					{Key: "as", Value: "latest_readings"},
				}},
			},
			bson.D{
				{Key: "$unwind", Value: bson.D{
					{Key: "path", Value: "$latest_readings"},
					{Key: "preserveNullAndEmptyArrays", Value: true},
				}},
			},
			bson.D{
				{Key: "$project", Value: bson.D{
					{Key: "chat_id", Value: 1},
					{Key: "apartment_number", Value: 1},
					{Key: "username", Value: 1},
					{Key: "cold", Value: "$latest_readings.cold"},
					{Key: "hot", Value: "$latest_readings.hot"},
					{Key: "date", Value: "$latest_readings.date"},
				}},
			},
		}

		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		cursor, err := app.UsersCollection.Aggregate(ctx, pipeline)
		if err != nil {
			log.Printf("Ошибка агрегации: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Ошибка запроса к базе данных"})
			return
		}
		defer cursor.Close(ctx)

		var results []models.LatestReadingResponse
		if err := cursor.All(ctx, &results); err != nil {
			log.Printf("Ошибка декодирования: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Ошибка обработки данных"})
			return
		}

		c.JSON(http.StatusOK, results)
	}
}

// Обновленная функция получения статистики
func getStatistics(app *models.AppContext, apartmentNumber int) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), config.DATABASE_TIMEOUT)
	defer cancel()

	// Pipeline для агрегации последних показаний за каждый месяц
	pipeline := mongo.Pipeline{
		bson.D{
			{Key: "$match", Value: bson.D{{Key: "apartment_number", Value: apartmentNumber}}},
		},
		bson.D{
			{Key: "$project", Value: bson.D{
				{Key: "year", Value: bson.D{{Key: "$year", Value: "$date"}}},
				{Key: "month", Value: bson.D{{Key: "$month", Value: "$date"}}},
				{Key: "cold", Value: 1},
				{Key: "hot", Value: 1},
				{Key: "date", Value: 1},
			}},
		},
		bson.D{
			{Key: "$sort", Value: bson.D{{Key: "date", Value: 1}}},
		},
		bson.D{
			{Key: "$group", Value: bson.D{
				{Key: "_id", Value: bson.D{
					{Key: "year", Value: "$year"},
					{Key: "month", Value: "$month"},
				}},
				{Key: "lastCold", Value: bson.D{{Key: "$last", Value: "$cold"}}},
				{Key: "lastHot", Value: bson.D{{Key: "$last", Value: "$hot"}}},
				{Key: "lastDate", Value: bson.D{{Key: "$last", Value: "$date"}}},
			}},
		},
		bson.D{
			{Key: "$sort", Value: bson.D{{Key: "lastDate", Value: 1}}},
		},
	}
	cursor, err := app.ReadingsCollection.Aggregate(ctx, pipeline)
	if err != nil {
		return "", fmt.Errorf("ошибка агрегации показаний: %v", err)
	}
	defer cursor.Close(ctx)

	var monthlyReadings []struct {
		ID struct {
			Year  int `bson:"year"`
			Month int `bson:"month"`
		} `bson:"_id"`
		LastCold float64   `bson:"lastCold"`
		LastHot  float64   `bson:"lastHot"`
		LastDate time.Time `bson:"lastDate"`
	}

	if err = cursor.All(ctx, &monthlyReadings); err != nil {
		return "", fmt.Errorf("ошибка декодирования данных: %v", err)
	}

	if len(monthlyReadings) < 2 {
		return "Для расчета статистики требуется минимум 2 месяца данных", nil
	}

	var (
		totalCold, totalHot       float64
		coldConsums, hotConsums   []float64
		maxCold, maxHot           float64
		minCold, minHot           float64 = math.MaxFloat64, math.MaxFloat64
		maxColdMonth, maxHotMonth time.Time
		minColdMonth, minHotMonth time.Time
	)

	prevCold := monthlyReadings[0].LastCold
	prevHot := monthlyReadings[0].LastHot

	for i := 1; i < len(monthlyReadings); i++ {
		current := monthlyReadings[i]

		coldDiff := current.LastCold - prevCold
		hotDiff := current.LastHot - prevHot

		if coldDiff < 0 {
			coldDiff = 0
		}
		if hotDiff < 0 {
			hotDiff = 0
		}

		// Обновляем общие суммы
		totalCold += coldDiff
		totalHot += hotDiff

		// Холодная вода
		coldConsums = append(coldConsums, coldDiff)
		if coldDiff > maxCold {
			maxCold = coldDiff
			maxColdMonth = current.LastDate
		}
		if coldDiff < minCold {
			minCold = coldDiff
			minColdMonth = current.LastDate
		}

		// Горячая вода
		hotConsums = append(hotConsums, hotDiff)
		if hotDiff > maxHot {
			maxHot = hotDiff
			maxHotMonth = current.LastDate
		}
		if hotDiff < minHot {
			minHot = hotDiff
			minHotMonth = current.LastDate
		}

		prevCold = current.LastCold
		prevHot = current.LastHot
	}

	// Рассчитываем средние значения
	avgCold := totalCold / float64(len(coldConsums))
	avgHot := totalHot / float64(len(hotConsums))

	// Форматируем даты
	formatDate := func(t time.Time) string {
		return t.Format("01.2006")
	}

	return fmt.Sprintf(
		config.MSG_STATISTICS,
		apartmentNumber,
		totalCold,
		totalHot,
		avgCold,
		avgHot,
		maxCold, formatDate(maxColdMonth),
		maxHot, formatDate(maxHotMonth),
		minCold, formatDate(minColdMonth),
		minHot, formatDate(minHotMonth),
	), nil
}

// Экспорт показаний в CSV
func exportReadingsToCSV(app *models.AppContext, apartmentNumber int) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), config.DATABASE_TIMEOUT)
	defer cancel()

	filter := bson.M{"apartment_number": apartmentNumber}
	options := options.Find().SetSort(bson.M{"date": 1})

	cursor, err := app.ReadingsCollection.Find(ctx, filter, options)
	if err != nil {
		return "", fmt.Errorf("ошибка получения показаний: %v", err)
	}
	defer cursor.Close(ctx)

	var readings []models.Reading
	if err = cursor.All(ctx, &readings); err != nil {
		return "", fmt.Errorf("ошибка чтения показаний: %v", err)
	}

	if len(readings) == 0 {
		return "", fmt.Errorf("нет показаний для экспорта")
	}

	// Создаем временный файл CSV
	fileName := fmt.Sprintf("readings_apt%d_%s.csv", apartmentNumber, time.Now().Format("20060102_150405"))
	filePath := filepath.Join(os.TempDir(), fileName)

	file, err := os.Create(filePath)
	if err != nil {
		return "", fmt.Errorf("ошибка создания файла: %v", err)
	}
	defer file.Close()

	// Добавляем BOM для корректного отображения кириллицы в Excel
	file.Write([]byte{0xEF, 0xBB, 0xBF})

	writer := csv.NewWriter(file)
	defer writer.Flush()

	// Заголовок CSV
	header := []string{"Дата", "Холодная вода", "Горячая вода", "Потребление холодной", "Потребление горячей"}
	if err := writer.Write(header); err != nil {
		return "", fmt.Errorf("ошибка записи заголовка: %v", err)
	}

	// Добавляем данные
	var prevCold, prevHot float64
	for _, r := range readings {
		// Расчет потребления
		coldConsumption := r.Cold - prevCold
		hotConsumption := r.Hot - prevHot

		if prevCold > 0 && prevHot > 0 {
			row := []string{
				r.Date.Format("02.01.2006"),
				fmt.Sprintf("%.2f", r.Cold),
				fmt.Sprintf("%.2f", r.Hot),
				fmt.Sprintf("%.2f", coldConsumption),
				fmt.Sprintf("%.2f", hotConsumption),
			}

			if err := writer.Write(row); err != nil {
				return "", fmt.Errorf("ошибка записи данных: %v", err)
			}
		} else {
			// Для первой записи просто добавляем показания без потребления
			row := []string{
				r.Date.Format("02.01.2006"),
				fmt.Sprintf("%.2f", r.Cold),
				fmt.Sprintf("%.2f", r.Hot),
				"",
				"",
			}

			if err := writer.Write(row); err != nil {
				return "", fmt.Errorf("ошибка записи данных: %v", err)
			}
		}

		prevCold = r.Cold
		prevHot = r.Hot
	}

	return filePath, nil
}

// Создание резервной копии базы данных
func createBackup(app *models.AppContext) error {
	ctx, cancel := context.WithTimeout(context.Background(), config.DATABASE_TIMEOUT*2)
	defer cancel()
	// Получаем время последнего бэкапа из конфига
	var lastBackupTime time.Time
	err := app.ConfigCollection.FindOne(ctx, bson.M{}).Decode(&app.Config)
	if err != nil {
		return fmt.Errorf("ошибка чтения конфига: %v", err)
	}
	lastBackupTime = app.Config.LastBackupTime

	backupTime := time.Now().Format("20060102_150405")
	backupDir := filepath.Join(config.BACKUP_FOLDER, backupTime)
	if err := os.MkdirAll(backupDir, 0o755); err != nil {
		return fmt.Errorf("ошибка создания директории бэкапа: %v", err)
	}

	collections := []struct {
		Collection *mongo.Collection
		Name       string
	}{
		{app.UsersCollection, "users"},
		{app.ReadingsCollection, "readings"},
		{app.NewsCollection, "news"},
		{app.AdminsCollection, "admins"},
		{app.ConfigCollection, "config"},
		{app.ApartmentsCollection, "apartments"},
	}
	for _, coll := range collections {
		filePath := filepath.Join(backupDir, coll.Name+".json")
		file, err := os.Create(filePath)
		if err != nil {
			return fmt.Errorf("ошибка создания файла %s: %v", filePath, err)
		}
		defer file.Close()

		// Фильтр: документы изменены после последнего бэкапа
		filter := bson.M{"updated_at": bson.M{"$gte": lastBackupTime}}
		cursor, err := coll.Collection.Find(ctx, filter)
		if err != nil {
			return fmt.Errorf("ошибка чтения коллекции %s: %v", coll.Name, err)
		}

		var documents []bson.M
		if err = cursor.All(ctx, &documents); err != nil {
			cursor.Close(ctx)
			return fmt.Errorf("ошибка декодирования документов из %s: %v", coll.Name, err)
		}
		cursor.Close(ctx)

		for _, doc := range documents {
			jsonData, err := bson.MarshalExtJSON(doc, true, false)
			if err != nil {
				return fmt.Errorf("ошибка маршалинга JSON для %s: %v", coll.Name, err)
			}
			file.Write(jsonData)
			file.Write([]byte("\n"))
		}
	}

	// Обновляем время последнего бэкапа в конфиге
	update := bson.M{"$set": bson.M{"last_backup_time": time.Now()}}
	_, err = app.ConfigCollection.UpdateOne(ctx, bson.M{}, update)
	if err != nil {
		return fmt.Errorf("ошибка обновления конфига: %v", err)
	}

	log.Printf("Инкрементальный бэкап создан: %s", backupDir)
	return nil
}

// Добавьте эту функцию на уровне пакета (не внутри других функций)
func checkUserExistsError(app *models.AppContext, chatID int64) error {
	_, err := getUser(app, chatID)
	return err
}

// Добавьте эту функцию на уровне пакета (не внутри других функций)
func checkUserExists(app *models.AppContext, chatID int64) bool {
	_, err := getUser(app, chatID)
	return err == nil
}

// Периодические задачи
func startScheduledTasks(app *models.AppContext) {
	// Ежедневная проверка нужно ли отправлять напоминания
	go func() {
		for {
			now := time.Now()
			// Если сегодня день для напоминаний (из настроек)
			if now.Day() == app.Config.ReadingReminderDay {
				// Проверяем, что сейчас утро (9:00)
				if now.Hour() == 9 && now.Minute() < 15 {
					log.Printf("Запуск автоматической отправки напоминаний")
					count, err := sendReminders(app)
					if err != nil {
						log.Printf("Ошибка отправки напоминаний: %v", err)
					} else {
						log.Printf("Напоминания отправлены: %d", count)
					}
				}
			}

			// Ждем один час перед следующей проверкой
			time.Sleep(1 * time.Hour)
		}
	}()

	// Регулярное резервное копирование
	go func() {
		for {
			if err := createBackup(app); err != nil {
				log.Printf("Ошибка создания резервной копии: %v", err)
			}

			// Удаляем старые резервные копии (старше 30 дней)
			cleanupBackups()

			// Ждем интервал из настроек
			time.Sleep(time.Duration(app.Config.BackupIntervalHours) * time.Hour)
		}
	}()
}

// Удаление старых резервных копий
func cleanupBackups() {
	// Максимальный возраст бэкапов - 30 дней
	maxAge := 30 * 24 * time.Hour

	entries, err := os.ReadDir(config.BACKUP_FOLDER)
	if err != nil {
		log.Printf("Ошибка чтения директории бэкапов: %v", err)
		return
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		info, err := entry.Info()
		if err != nil {
			continue
		}

		// Если директория старше maxAge, удаляем её
		if time.Since(info.ModTime()) > maxAge {
			backupPath := filepath.Join(config.BACKUP_FOLDER, entry.Name())
			if err := os.RemoveAll(backupPath); err != nil {
				log.Printf("Ошибка удаления старого бэкапа %s: %v", backupPath, err)
			} else {
				log.Printf("Удален старый бэкап: %s", backupPath)
			}
		}
	}
}

// Создание клавиатуры для подтверждения
func createConfirmationKeyboard() tgbotapi.InlineKeyboardMarkup {
	return tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("✅ Подтвердить", "confirm"),
			tgbotapi.NewInlineKeyboardButtonData("❌ Отменить", "cancel"),
		),
	)
}

// Обработка команды /start
func handleStart(app *models.AppContext, update tgbotapi.Update) {
	chatID := update.Message.Chat.ID
	//	username := update.Message.From.UserName

	// Отправляем приветственное сообщение
	msg := tgbotapi.NewMessage(chatID, config.MSG_START)
	app.Bot.Send(msg)

	// Проверка режима обслуживания
	if app.Config.MaintenanceMode && !isAdmin(app, chatID) {
		msg := tgbotapi.NewMessage(chatID, app.Config.MaintenanceMessage)
		app.Bot.Send(msg)
		return
	}

	isRegistered, err := isUserRegistered(app, chatID)
	if err != nil {
		log.Printf("Ошибка проверки регистрации: %v", err)
		msg := tgbotapi.NewMessage(chatID, config.MSG_ERROR_OCCURRED)
		app.Bot.Send(msg)
		return
	}

	if isRegistered {
		// Получаем данные пользователя
		user, err := getUser(app, chatID)
		if err != nil {
			log.Printf("Ошибка получения данных пользователя: %v", err)
			msg := tgbotapi.NewMessage(chatID, config.MSG_ERROR_OCCURRED)
			app.Bot.Send(msg)
			return
		}

		// Обновляем активность пользователя
		setUserState(app, chatID, config.STATE_WAITING_READINGS)
		updateUserActivity(app, chatID)

		// Пользователь уже зарегистрирован
		msg := tgbotapi.NewMessage(chatID, fmt.Sprintf("Вы уже зарегистрированы как житель квартиры %d. Введите показания счетчиков (холодная и горячая вода) через пробел:", user.ApartmentNumber))
		app.Bot.Send(msg)
	} else {
		// Пользователь не зарегистрирован
		setUserState(app, chatID, config.STATE_WAITING_APARTMENT)
		msg := tgbotapi.NewMessage(
			chatID,
			fmt.Sprintf(config.MSG_START_NOT_REGISTERED, config.MIN_APARTMENT_NUMBER, config.MAX_APARTMENT_NUMBER),
		)
		app.Bot.Send(msg)
	}
}

// Обработка команды /history
func handleHistory(app *models.AppContext, update tgbotapi.Update) {
	chatID := update.Message.Chat.ID

	// Проверка режима обслуживания
	if app.Config.MaintenanceMode && !isAdmin(app, chatID) {
		msg := tgbotapi.NewMessage(chatID, app.Config.MaintenanceMessage)
		app.Bot.Send(msg)
		return
	}

	// Проверяем, зарегистрирован ли пользователь
	isRegistered, err := isUserRegistered(app, chatID)
	if err != nil {
		log.Printf("Ошибка проверки регистрации: %v", err)
		msg := tgbotapi.NewMessage(chatID, config.MSG_ERROR_OCCURRED)
		app.Bot.Send(msg)
		return
	}

	if !isRegistered {
		msg := tgbotapi.NewMessage(chatID, "Вы не зарегистрированы. Введите /start для начала.")
		app.Bot.Send(msg)
		return
	}

	// Получаем данные пользователя
	user, err := getUser(app, chatID)
	if err != nil {
		log.Printf("Ошибка получения данных пользователя: %v", err)
		msg := tgbotapi.NewMessage(chatID, config.MSG_ERROR_OCCURRED)
		app.Bot.Send(msg)
		return
	}

	// Получаем историю показаний
	history, err := getHistory(app, user.ApartmentNumber, 12)
	if err != nil {
		log.Printf("Ошибка получения истории показаний: %v", err)
		msg := tgbotapi.NewMessage(chatID, config.MSG_ERROR_OCCURRED)
		app.Bot.Send(msg)
		return
	}

	// Отправляем историю пользователю
	msg := tgbotapi.NewMessage(chatID, history)
	app.Bot.Send(msg)
}

// Обработка команды /stats
func handleStats(app *models.AppContext, update tgbotapi.Update) {
	chatID := update.Message.Chat.ID

	// Проверка режима обслуживания
	if app.Config.MaintenanceMode && !isAdmin(app, chatID) {
		msg := tgbotapi.NewMessage(chatID, app.Config.MaintenanceMessage)
		app.Bot.Send(msg)
		return
	}

	// Проверяем, зарегистрирован ли пользователь
	isRegistered, err := isUserRegistered(app, chatID)
	if err != nil {
		log.Printf("Ошибка проверки регистрации: %v", err)
		msg := tgbotapi.NewMessage(chatID, config.MSG_ERROR_OCCURRED)
		app.Bot.Send(msg)
		return
	}

	if !isRegistered {
		msg := tgbotapi.NewMessage(chatID, "Вы не зарегистрированы. Введите /start для начала.")
		app.Bot.Send(msg)
		return
	}

	// Получаем данные пользователя
	user, err := getUser(app, chatID)
	if err != nil {
		log.Printf("Ошибка получения данных пользователя: %v", err)
		msg := tgbotapi.NewMessage(chatID, config.MSG_ERROR_OCCURRED)
		app.Bot.Send(msg)
		return
	}

	// Получаем статистику
	stats, err := getStatistics(app, user.ApartmentNumber)
	if err != nil {
		log.Printf("Ошибка получения статистики: %v", err)
		msg := tgbotapi.NewMessage(chatID, config.MSG_ERROR_OCCURRED)
		app.Bot.Send(msg)
		return
	}

	// Отправляем статистику пользователю
	msg := tgbotapi.NewMessage(chatID, stats)
	app.Bot.Send(msg)
}

// Обработка регистрации нового пользователя
func handleRegistration(app *models.AppContext, update tgbotapi.Update) {
	chatID := update.Message.Chat.ID
	messageText := update.Message.Text

	// Проверяем формат номера квартиры
	apartmentNumberStr := strings.TrimSpace(messageText)
	apartmentNumber, err := strconv.Atoi(apartmentNumberStr)
	if err != nil || apartmentNumber < config.MIN_APARTMENT_NUMBER || apartmentNumber > config.MAX_APARTMENT_NUMBER {
		msg := tgbotapi.NewMessage(chatID, fmt.Sprintf(config.MSG_INVALID_APARTMENT, config.MIN_APARTMENT_NUMBER, config.MAX_APARTMENT_NUMBER))
		app.Bot.Send(msg)
		return
	}

	// Создаем нового пользователя
	err = registerUser(app, chatID, update.Message.From.UserName, apartmentNumber)
	if err != nil {
		log.Printf("Ошибка регистрации: %v", err)
		msg := tgbotapi.NewMessage(chatID, "Ошибка: "+err.Error())
		app.Bot.Send(msg)
		return
	}

	// Отправляем сообщение об успешной регистрации
	msg := tgbotapi.NewMessage(chatID, fmt.Sprintf(config.MSG_REGISTRATION_SUCCESS, apartmentNumber))
	app.Bot.Send(msg)

	// Просим пользователя ввести показания
	msg = tgbotapi.NewMessage(chatID, config.MSG_START_REGISTERED)
	app.Bot.Send(msg)
}

// Обработка команды /export
func handleExport(app *models.AppContext, update tgbotapi.Update) {
	chatID := update.Message.Chat.ID

	// Проверка режима обслуживания
	if app.Config.MaintenanceMode && !isAdmin(app, chatID) {
		msg := tgbotapi.NewMessage(chatID, app.Config.MaintenanceMessage)
		app.Bot.Send(msg)
		return
	}

	// Проверяем, зарегистрирован ли пользователь
	isRegistered, err := isUserRegistered(app, chatID)
	if err != nil {
		log.Printf("Ошибка проверки регистрации: %v", err)
		msg := tgbotapi.NewMessage(chatID, config.MSG_ERROR_OCCURRED)
		app.Bot.Send(msg)
		return
	}

	if !isRegistered {
		msg := tgbotapi.NewMessage(chatID, "Вы не зарегистрированы. Введите /start для начала.")
		app.Bot.Send(msg)
		return
	}

	// Получаем данные пользователя
	user, err := getUser(app, chatID)
	if err != nil {
		log.Printf("Ошибка получения данных пользователя: %v", err)
		msg := tgbotapi.NewMessage(chatID, config.MSG_ERROR_OCCURRED)
		app.Bot.Send(msg)
		return
	}

	// Экспортируем данные в CSV
	filePath, err := exportReadingsToCSV(app, user.ApartmentNumber)
	if err != nil {
		log.Printf("Ошибка экспорта данных: %v", err)
		msg := tgbotapi.NewMessage(chatID, "Не удалось экспортировать данные: "+err.Error())
		app.Bot.Send(msg)
		return
	}

	// Отправляем файл пользователю
	doc := tgbotapi.NewDocument(chatID, tgbotapi.FilePath(filePath))
	app.Bot.Send(doc)

	// Удаляем временный файл
	os.Remove(filePath)
}

// Обработка команды /addnews (только для админов)
func handleAddNews(app *models.AppContext, update tgbotapi.Update) {
	chatID := update.Message.Chat.ID

	// Проверяем, является ли пользователь админом
	if !isAdmin(app, chatID) {
		msg := tgbotapi.NewMessage(chatID, config.MSG_NO_PERMISSION)
		app.Bot.Send(msg)
		return
	}
	// Устанавливаем состояние ожидания ввода новости
	setUserState(app, chatID, config.STATE_ADMIN_WAITING_NEWS)
	// Проверяем формат команды
	args := strings.TrimSpace(update.Message.CommandArguments())
	parts := strings.SplitN(args, " ", 2)
	if len(parts) != 2 {
		msg := tgbotapi.NewMessage(chatID, config.MSG_ADDNEWS_USAGE)
		app.Bot.Send(msg)
		return
	}

	title := parts[0]
	content := parts[1]
	username := update.Message.From.UserName

	// Добавляем новость
	err := addNews(app, title, content, username)
	if err != nil {
		log.Printf("Ошибка добавления новости: %v", err)
		msg := tgbotapi.NewMessage(chatID, "Ошибка: "+err.Error())
		app.Bot.Send(msg)
		return
	}

	// Отправляем подтверждение
	msg := tgbotapi.NewMessage(chatID, config.MSG_NEWS_ADDED)
	app.Bot.Send(msg)
}

// Обработка команды /sendnews (только для админов)
func handleSendNews(app *models.AppContext, update tgbotapi.Update) {
	chatID := update.Message.Chat.ID

	// Проверяем, является ли пользователь админом
	if !isAdmin(app, chatID) {
		msg := tgbotapi.NewMessage(chatID, config.MSG_NO_PERMISSION)
		app.Bot.Send(msg)
		return
	}

	// Отправляем сообщение о начале процесса
	statusMsg := tgbotapi.NewMessage(chatID, "Начинаем отправку новостей пользователям...")
	sentMsg, _ := app.Bot.Send(statusMsg)

	// Отправляем новости всем пользователям
	count, err := sendNewsToUsers(app)
	if err != nil {
		log.Printf("Ошибка отправки новостей: %v", err)
		edit := tgbotapi.NewEditMessageText(chatID, sentMsg.MessageID, "Ошибка отправки новостей: "+err.Error())
		app.Bot.Send(edit)
		return
	}

	// Обновляем статусное сообщение
	edit := tgbotapi.NewEditMessageText(
		chatID,
		sentMsg.MessageID,
		fmt.Sprintf("Новости успешно отправлены. Количество отправленных сообщений: %d", count),
	)
	app.Bot.Send(edit)
}

// Обработка команды /remind (только для админов)
func handleRemind(app *models.AppContext, update tgbotapi.Update) {
	chatID := update.Message.Chat.ID

	// Проверяем, является ли пользователь админом
	if !isAdmin(app, chatID) {
		msg := tgbotapi.NewMessage(chatID, config.MSG_NO_PERMISSION)
		app.Bot.Send(msg)
		return
	}

	// Отправляем сообщение о начале процесса
	statusMsg := tgbotapi.NewMessage(chatID, "Начинаем отправку напоминаний...")
	sentMsg, _ := app.Bot.Send(statusMsg)

	// Отправляем напоминания
	count, err := sendReminders(app)
	if err != nil {
		log.Printf("Ошибка отправки напоминаний: %v", err)
		edit := tgbotapi.NewEditMessageText(chatID, sentMsg.MessageID, "Ошибка отправки напоминаний: "+err.Error())
		app.Bot.Send(edit)
		return
	}

	// Обновляем статусное сообщение
	edit := tgbotapi.NewEditMessageText(
		chatID,
		sentMsg.MessageID,
		fmt.Sprintf("Напоминания успешно отправлены. Количество отправленных сообщений: %d", count),
	)
	app.Bot.Send(edit)
}

// Обработка команды /backup (только для админов)
func handleBackup(app *models.AppContext, update tgbotapi.Update) {
	chatID := update.Message.Chat.ID

	// Проверяем, является ли пользователь админом
	if !isAdmin(app, chatID) {
		msg := tgbotapi.NewMessage(chatID, config.MSG_NO_PERMISSION)
		app.Bot.Send(msg)
		return
	}

	// Отправляем сообщение о начале процесса
	statusMsg := tgbotapi.NewMessage(chatID, "Начинаем создание резервной копии...")
	sentMsg, _ := app.Bot.Send(statusMsg)

	// Создаем резервную копию
	err := createBackup(app)
	if err != nil {
		log.Printf("Ошибка создания резервной копии: %v", err)
		edit := tgbotapi.NewEditMessageText(chatID, sentMsg.MessageID, "Ошибка создания резервной копии: "+err.Error())
		app.Bot.Send(edit)
		return
	}

	// Обновляем статусное сообщение
	edit := tgbotapi.NewEditMessageText(chatID, sentMsg.MessageID, config.MSG_BACKUP_COMPLETED)
	app.Bot.Send(edit)
}

// Обработка команды /maintenance (только для админов)
func handleMaintenance(app *models.AppContext, update tgbotapi.Update) {
	chatID := update.Message.Chat.ID

	// Проверяем, является ли пользователь админом
	if !isAdmin(app, chatID) {
		msg := tgbotapi.NewMessage(chatID, config.MSG_NO_PERMISSION)
		app.Bot.Send(msg)
		return
	}

	// Получаем аргументы команды
	args := strings.TrimSpace(update.Message.CommandArguments())

	ctx, cancel := context.WithTimeout(context.Background(), config.DATABASE_TIMEOUT)
	defer cancel()

	if args == "on" {
		// Включаем режим обслуживания
		_, err := app.ConfigCollection.UpdateOne(
			ctx,
			bson.M{},
			bson.M{"$set": bson.M{"maintenance_mode": true}},
		)
		if err != nil {
			log.Printf("Ошибка включения режима обслуживания: %v", err)
			msg := tgbotapi.NewMessage(chatID, "Ошибка: "+err.Error())
			app.Bot.Send(msg)
			return
		}

		app.Config.MaintenanceMode = true
		msg := tgbotapi.NewMessage(chatID, "Режим обслуживания включен.")
		app.Bot.Send(msg)
	} else if args == "off" {
		// Выключаем режим обслуживания
		_, err := app.ConfigCollection.UpdateOne(
			ctx,
			bson.M{},
			bson.M{"$set": bson.M{"maintenance_mode": false}},
		)
		if err != nil {
			log.Printf("Ошибка выключения режима обслуживания: %v", err)
			msg := tgbotapi.NewMessage(chatID, "Ошибка: "+err.Error())
			app.Bot.Send(msg)
			return
		}

		app.Config.MaintenanceMode = false
		msg := tgbotapi.NewMessage(chatID, "Режим обслуживания выключен.")
		app.Bot.Send(msg)
	} else {
		// Отображаем текущий статус
		status := "выключен"
		if app.Config.MaintenanceMode {
			status = "включен"
		}

		msg := tgbotapi.NewMessage(
			chatID,
			fmt.Sprintf("Текущий статус режима обслуживания: %s\n\nИспользование:\n/maintenance on - включить\n/maintenance off - выключить", status),
		)
		app.Bot.Send(msg)
	}
}

// Обработка новых показаний
func handleReadings(app *models.AppContext, update tgbotapi.Update) {
	chatID := update.Message.Chat.ID
	messageText := update.Message.Text

	// Проверка режима обслуживания
	if app.Config.MaintenanceMode && !isAdmin(app, chatID) {
		msg := tgbotapi.NewMessage(chatID, app.Config.MaintenanceMessage)
		app.Bot.Send(msg)
		return
	}
	// Обработка показаний счетчиков
	parts := strings.Fields(messageText)
	if len(parts) != 2 {
		msg := tgbotapi.NewMessage(chatID, config.MSG_INVALID_FORMAT)
		app.Bot.Send(msg)
		return
	}

	// Проверяем корректность формата данных
	cold, err := strconv.ParseFloat(parts[0], 64)
	if err != nil || cold <= 0 {
		msg := tgbotapi.NewMessage(chatID, config.MSG_VALUE_MUST_BE_POSITIVE)
		app.Bot.Send(msg)
		return
	}

	hot, err := strconv.ParseFloat(parts[1], 64)
	if err != nil || hot <= 0 {
		msg := tgbotapi.NewMessage(chatID, config.MSG_VALUE_MUST_BE_POSITIVE)
		app.Bot.Send(msg)
		return
	}

	// Проверяем, что значения не превышают максимально допустимые
	if cold > config.MAX_WATER_VALUE || hot > config.MAX_WATER_VALUE {
		msg := tgbotapi.NewMessage(chatID, fmt.Sprintf("Значение не может быть больше %.0f.", config.MAX_WATER_VALUE))
		app.Bot.Send(msg)
		return
	}

	// Получаем данные пользователя
	user, err := getUser(app, chatID)
	if err != nil {
		log.Printf("Ошибка получения данных пользователя: %v", err)
		msg := tgbotapi.NewMessage(chatID, config.MSG_ERROR_OCCURRED)
		app.Bot.Send(msg)
		return
	}

	// Получаем последние показания для квартиры
	//	lastCold, lastHot, lastDate, err := getLastReadings(app, user.ApartmentNumber)
	lastCold, lastHot, _, err := getLastReadings(app, user.ApartmentNumber)
	if err != nil {
		log.Printf("Ошибка получения последних показаний: %v", err)
		msg := tgbotapi.NewMessage(chatID, config.MSG_ERROR_OCCURRED)
		app.Bot.Send(msg)
		return
	}

	// Проверяем, что новые показания больше или равны предыдущим
	if lastCold > 0 && cold < lastCold {
		msg := tgbotapi.NewMessage(chatID, fmt.Sprintf(config.MSG_COLD_DECREASED, cold, lastCold))
		app.Bot.Send(msg)
		return
	}

	if lastHot > 0 && hot < lastHot {
		msg := tgbotapi.NewMessage(chatID, fmt.Sprintf(config.MSG_HOT_DECREASED, hot, lastHot))
		app.Bot.Send(msg)
		return
	}

	// Проверяем разницу между новыми и предыдущими показаниями
	if lastCold > 0 && (cold-lastCold) > app.Config.MaxColdWaterIncrease {
		// Показания вызывают подозрение, запрашиваем подтверждение
		msg := tgbotapi.NewMessage(
			chatID,
			fmt.Sprintf(config.MSG_LARGE_DIFFERENCE_COLD, cold-lastCold)+"\n\nХолодная вода: "+fmt.Sprintf("%.2f → %.2f", lastCold, cold),
		)
		msg.ReplyMarkup = createConfirmationKeyboard()
		app.Bot.Send(msg)
		return
	}

	if lastHot > 0 && (hot-lastHot) > app.Config.MaxHotWaterIncrease {
		// Показания вызывают подозрение, запрашиваем подтверждение
		msg := tgbotapi.NewMessage(
			chatID,
			fmt.Sprintf(config.MSG_LARGE_DIFFERENCE_HOT, hot-lastHot)+"\n\nГорячая вода: "+fmt.Sprintf("%.2f → %.2f", lastHot, hot),
		)
		msg.ReplyMarkup = createConfirmationKeyboard()
		app.Bot.Send(msg)
		return
	}

	// Проверка периода сдачи показаний
	now := time.Now()
	currentDay := now.Day()
	if currentDay < app.Config.AllowReadingsFrom || currentDay > app.Config.AllowReadingsTo {
		// Предупреждаем, что период сдачи показаний не оптимальный, но разрешаем сохранить
		warningText := fmt.Sprintf("⚠️ Рекомендуемый период сдачи показаний: с %d по %d число каждого месяца. Вы уверены, что хотите сохранить показания сейчас?",
			app.Config.AllowReadingsFrom, app.Config.AllowReadingsTo)

		msg := tgbotapi.NewMessage(chatID, warningText)
		msg.ReplyMarkup = createConfirmationKeyboard()
		app.Bot.Send(msg)
		return
	}

	// Сохраняем показания
	receivedTime, err := saveReadings(app, user.ApartmentNumber, chatID, cold, hot)
	if err != nil {
		log.Printf("Ошибка сохранения показаний: %v", err)
		msg := tgbotapi.NewMessage(chatID, "Ошибка сохранения показаний: "+err.Error())
		app.Bot.Send(msg)
		return
	}

	// Обновляем время активности пользователя
	updateUserActivity(app, chatID)

	// Отправляем сообщение об успешном сохранении
	msg := tgbotapi.NewMessage(chatID, fmt.Sprintf(config.MSG_READINGS_SUCCESS, cold, hot, receivedTime))
	app.Bot.Send(msg)
}

// Обработка ввода показаний
func handleReadingsInput(app *models.AppContext, update tgbotapi.Update) {
	chatID := update.Message.Chat.ID
	messageText := update.Message.Text

	// Получаем данные пользователя
	user, err := getUser(app, chatID)
	if err != nil {
		log.Printf("Ошибка получения данных пользователя: %v", err)
		msg := tgbotapi.NewMessage(chatID, config.MSG_ERROR_OCCURRED)
		app.Bot.Send(msg)
		return
	}

	// Обработка показаний счетчиков
	parts := strings.Fields(messageText)
	if len(parts) != 2 {
		msg := tgbotapi.NewMessage(chatID, config.MSG_INVALID_FORMAT)
		app.Bot.Send(msg)
		return
	}

	// Проверяем корректность формата данных
	cold, err := strconv.ParseFloat(parts[0], 64)
	if err != nil || cold <= 0 {
		msg := tgbotapi.NewMessage(chatID, config.MSG_VALUE_MUST_BE_POSITIVE)
		app.Bot.Send(msg)
		return
	}

	hot, err := strconv.ParseFloat(parts[1], 64)
	if err != nil || hot <= 0 {
		msg := tgbotapi.NewMessage(chatID, config.MSG_VALUE_MUST_BE_POSITIVE)
		app.Bot.Send(msg)
		return
	}

	// Проверяем, что значения не превышают максимально допустимые
	if cold > config.MAX_WATER_VALUE || hot > config.MAX_WATER_VALUE {
		msg := tgbotapi.NewMessage(chatID, fmt.Sprintf("Значение не может быть больше %.0f.", config.MAX_WATER_VALUE))
		app.Bot.Send(msg)
		return
	}

	// Получаем последние показания для квартиры
	lastCold, lastHot, _, err := getLastReadings(app, user.ApartmentNumber)
	if err != nil {
		log.Printf("Ошибка получения последних показаний: %v", err)
		msg := tgbotapi.NewMessage(chatID, config.MSG_ERROR_OCCURRED)
		app.Bot.Send(msg)
		return
	}

	// Проверяем, что новые показания больше или равны предыдущим
	if lastCold > 0 && cold < lastCold {
		msg := tgbotapi.NewMessage(chatID, fmt.Sprintf(config.MSG_COLD_DECREASED, cold, lastCold))
		app.Bot.Send(msg)
		return
	}

	if lastHot > 0 && hot < lastHot {
		msg := tgbotapi.NewMessage(chatID, fmt.Sprintf(config.MSG_HOT_DECREASED, hot, lastHot))
		app.Bot.Send(msg)
		return
	}

	// Проверяем разницу между новыми и предыдущими показаниями
	if lastCold > 0 && (cold-lastCold) > app.Config.MaxColdWaterIncrease {
		// Показания вызывают подозрение, запрашиваем подтверждение
		setUserState(app, chatID, config.STATE_CONFIRMING_READINGS)
		msg := tgbotapi.NewMessage(
			chatID,
			fmt.Sprintf(config.MSG_LARGE_DIFFERENCE_COLD, cold-lastCold)+"\n\nХолодная вода: "+fmt.Sprintf("%.2f → %.2f", lastCold, cold),
		)
		msg.ReplyMarkup = createConfirmationKeyboard()
		app.Bot.Send(msg)
		return
	}

	if lastHot > 0 && (hot-lastHot) > app.Config.MaxHotWaterIncrease {
		// Показания вызывают подозрение, запрашиваем подтверждение
		setUserState(app, chatID, config.STATE_CONFIRMING_READINGS)
		msg := tgbotapi.NewMessage(
			chatID,
			fmt.Sprintf(config.MSG_LARGE_DIFFERENCE_HOT, hot-lastHot)+"\n\nГорячая вода: "+fmt.Sprintf("%.2f → %.2f", lastHot, hot),
		)
		msg.ReplyMarkup = createConfirmationKeyboard()
		app.Bot.Send(msg)
		return
	}

	// Сохраняем показания
	receivedTime, err := saveReadings(app, user.ApartmentNumber, chatID, cold, hot)
	if err != nil {
		log.Printf("Ошибка сохранения показаний: %v", err)
		msg := tgbotapi.NewMessage(chatID, "Ошибка сохранения показаний: "+err.Error())
		app.Bot.Send(msg)
		return
	}

	// Обновляем время активности пользователя
	updateUserActivity(app, chatID)

	// Отправляем сообщение об успешном сохранении
	msg := tgbotapi.NewMessage(chatID, fmt.Sprintf(config.MSG_READINGS_SUCCESS, cold, hot, receivedTime))
	app.Bot.Send(msg)
}

// Обработка нажатий на инлайн-кнопки
func handleCallbackQuery(app *models.AppContext, update tgbotapi.Update) {
	callback := update.CallbackQuery
	chatID := callback.Message.Chat.ID
	data := callback.Data
	// Обработка нажатий на инлайн-кнопки
	if strings.HasPrefix(data, "emergency_") {
		handleEmergencyCallback(app, update)
		return
	}
	// Отправляем уведомление о получении запроса
	app.Bot.Request(tgbotapi.NewCallback(callback.ID, ""))

	// Получаем текст исходного сообщения
	messageText := callback.Message.Text

	// Если пользователь отменил действие
	if data == "cancel" {
		// Редактируем сообщение
		edit := tgbotapi.NewEditMessageText(
			chatID,
			callback.Message.MessageID,
			messageText+"\n\n❌ Отменено пользователем",
		)
		edit.ReplyMarkup = nil
		app.Bot.Send(edit)

		// Сбрасываем состояние на ожидание показаний
		setUserState(app, chatID, config.STATE_WAITING_READINGS)
		return
	}
	if data == "confirm" {
		// Проверяем состояние пользователя
		state := getUserState(app, chatID)

		if state == config.STATE_CONFIRMING_READINGS {
			// Это подтверждение показаний счетчиков

			// Получаем пользователя
			user, err := getUser(app, chatID)
			if err != nil {
				log.Printf("Ошибка получения данных пользователя: %v", err)
				edit := tgbotapi.NewEditMessageText(
					chatID,
					callback.Message.MessageID,
					messageText+"\n\n❌ Ошибка: "+err.Error(),
				)
				edit.ReplyMarkup = nil
				app.Bot.Send(edit)

				// Сбрасываем состояние на ожидание показаний
				setUserState(app, chatID, config.STATE_WAITING_READINGS)
				return
			}

			// Пытаемся извлечь значения показаний из текста сообщения
			var cold, hot float64

			// Ищем показания в формате "X.XX → Y.YY"
			coldRangeIndex := strings.Index(messageText, "Холодная вода:")
			hotRangeIndex := strings.Index(messageText, "Горячая вода:")

			if coldRangeIndex > 0 {
				// Извлекаем значения для холодной воды
				coldRangeStr := messageText[coldRangeIndex:]
				coldRangeStr = coldRangeStr[:strings.Index(coldRangeStr, "\n")]
				parts := strings.Split(coldRangeStr, "→")
				if len(parts) > 1 {
					coldStr := strings.TrimSpace(parts[1])
					cold, _ = strconv.ParseFloat(coldStr, 64)
				}
			}

			if hotRangeIndex > 0 {
				// Извлекаем значения для горячей воды
				hotRangeStr := messageText[hotRangeIndex:]
				if strings.Contains(hotRangeStr, "\n") {
					hotRangeStr = hotRangeStr[:strings.Index(hotRangeStr, "\n")]
				}
				parts := strings.Split(hotRangeStr, "→")
				if len(parts) > 1 {
					hotStr := strings.TrimSpace(parts[1])
					hot, _ = strconv.ParseFloat(hotStr, 64)
				}
			}

			// Если не удалось извлечь из формата выше, пробуем извлечь из других частей текста
			if cold == 0 || hot == 0 {
				// Разбиваем текст на слова и ищем числа
				words := strings.Fields(messageText)
				for i, word := range words {
					if word == "Холодная:" && i+1 < len(words) {
						cold, _ = strconv.ParseFloat(words[i+1], 64)
					}
					if word == "Горячая:" && i+1 < len(words) {
						hot, _ = strconv.ParseFloat(words[i+1], 64)
					}
				}
			}

			// Проверяем, что удалось получить показания
			if cold == 0 || hot == 0 {
				edit := tgbotapi.NewEditMessageText(
					chatID,
					callback.Message.MessageID,
					messageText+"\n\n❌ Не удалось определить значения показаний",
				)
				edit.ReplyMarkup = nil
				app.Bot.Send(edit)
				return
			}

			// Сохраняем показания
			receivedTime, err := saveReadings(app, user.ApartmentNumber, chatID, cold, hot)
			if err != nil {
				log.Printf("Ошибка сохранения показаний: %v", err)
				edit := tgbotapi.NewEditMessageText(
					chatID,
					callback.Message.MessageID,
					messageText+"\n\n❌ Ошибка: "+err.Error(),
				)
				edit.ReplyMarkup = nil
				app.Bot.Send(edit)
				return
			}

			// Редактируем сообщение, удаляя кнопки и добавляя статус
			edit := tgbotapi.NewEditMessageText(
				chatID,
				callback.Message.MessageID,
				messageText+"\n\n✅ Показания подтверждены и сохранены!",
			)
			edit.ReplyMarkup = nil
			app.Bot.Send(edit)

			// Отправляем сообщение с подтверждением сохранения
			confirmMsg := tgbotapi.NewMessage(
				chatID,
				fmt.Sprintf(config.MSG_READINGS_SUCCESS, cold, hot, receivedTime),
			)
			app.Bot.Send(confirmMsg)
			setUserState(app, chatID, config.STATE_WAITING_READINGS)
		}
	}
}

// Обработка команды /emergency - создание аварийной заявки
func handleEmergency(app *models.AppContext, update tgbotapi.Update) {
	chatID := update.Message.Chat.ID

	// Проверка режима обслуживания
	if app.Config.MaintenanceMode && !isAdmin(app, chatID) {
		msg := tgbotapi.NewMessage(chatID, app.Config.MaintenanceMessage)
		app.Bot.Send(msg)
		return
	}

	// Проверяем, зарегистрирован ли пользователь
	isRegistered, err := isUserRegistered(app, chatID)
	if err != nil {
		log.Printf("Ошибка проверки регистрации: %v", err)
		msg := tgbotapi.NewMessage(chatID, config.MSG_ERROR_OCCURRED)
		app.Bot.Send(msg)
		return
	}

	if !isRegistered {
		msg := tgbotapi.NewMessage(chatID, "Вы не зарегистрированы. Введите /start для начала.")
		app.Bot.Send(msg)
		return
	}

	// Получаем данные пользователя
	err = checkUserExistsError(app, chatID)
	// Используем подчеркивание, если переменная не нужна
	if err != nil {
		log.Printf("Ошибка получения данных пользователя: %v", err)
		msg := tgbotapi.NewMessage(chatID, config.MSG_ERROR_OCCURRED)
		app.Bot.Send(msg)
		return
	}

	// Устанавливаем состояние ожидания описания аварийной ситуации
	setUserState(app, chatID, "EMERGENCY_WAITING_DESCRIPTION")

	// Запрашиваем описание проблемы
	msg := tgbotapi.NewMessage(chatID,
		"🚨 *Создание аварийной заявки* 🚨\n\n"+
			"Опишите проблему подробно. Укажите:\n"+
			"1. Что случилось?\n"+
			"2. Где именно в квартире произошла авария?\n"+
			"3. Есть ли угроза соседям?\n\n"+
			"Ваше сообщение будет передано ответственным лицам.")
	msg.ParseMode = "Markdown"
	app.Bot.Send(msg)
}

// Обработка команды /myemergency - просмотр своих аварийных заявок
func handleMyEmergency(app *models.AppContext, update tgbotapi.Update) {
	chatID := update.Message.Chat.ID

	// Проверка режима обслуживания
	if app.Config.MaintenanceMode && !isAdmin(app, chatID) {
		msg := tgbotapi.NewMessage(chatID, app.Config.MaintenanceMessage)
		app.Bot.Send(msg)
		return
	}

	// Проверяем, зарегистрирован ли пользователь
	isRegistered, err := isUserRegistered(app, chatID)
	if err != nil {
		log.Printf("Ошибка проверки регистрации: %v", err)
		msg := tgbotapi.NewMessage(chatID, config.MSG_ERROR_OCCURRED)
		app.Bot.Send(msg)
		return
	}

	if !isRegistered {
		msg := tgbotapi.NewMessage(chatID, "Вы не зарегистрированы. Введите /start для начала.")
		app.Bot.Send(msg)
		return
	}

	// Получаем аварийные заявки пользователя
	requests, err := getUserEmergencyRequests(app, chatID)
	if err != nil {
		log.Printf("Ошибка получения заявок: %v", err)
		msg := tgbotapi.NewMessage(chatID, config.MSG_ERROR_OCCURRED)
		app.Bot.Send(msg)
		return
	}

	if len(requests) == 0 {
		msg := tgbotapi.NewMessage(chatID, "У вас нет активных аварийных заявок.")
		app.Bot.Send(msg)
		return
	}

	// Формируем сообщение со списком заявок
	message := "📝 *Ваши аварийные заявки:*\n\n"

	for i, req := range requests {
		// Определяем статус для отображения
		var statusText string
		switch req.Status {
		case config.EMERGENCY_STATUS_NEW:
			statusText = "🆕 Новая"
		case config.EMERGENCY_STATUS_CONFIRMED:
			statusText = "✅ Подтверждена"
		case config.EMERGENCY_STATUS_IN_PROGRESS:
			statusText = "🛠 В работе"
		case config.EMERGENCY_STATUS_COMPLETED:
			statusText = "✓ Выполнена"
		case config.EMERGENCY_STATUS_REJECTED:
			statusText = "❌ Отклонена"
		default:
			statusText = req.Status
		}

		// Добавляем информацию о заявке
		message += fmt.Sprintf(
			"%d. ID: `%s`\n"+
				"Дата: %s\n"+
				"Статус: %s\n"+
				"Приоритет: %d/5\n"+
				"Описание: %s\n",
			i+1,
			req.ID.Hex(),
			req.Date.Format("02.01.2006 15:04"),
			statusText,
			req.Priority,
			utils.TruncateText(req.Description, 100), // Ограничиваем длину описания
		)

		// Добавляем информацию об исполнителе, если есть
		if req.AssignedTo != "" {
			message += fmt.Sprintf("Исполнитель: %s\n", req.AssignedTo)
		}

		// Добавляем разделитель между заявками
		if i < len(requests)-1 {
			message += "\n-------------------\n\n"
		}
	}

	msg := tgbotapi.NewMessage(chatID, message)
	msg.ParseMode = "Markdown"
	app.Bot.Send(msg)
}

// Обработка команды /allemergency - просмотр всех активных аварийных заявок (только для админов)
func handleAllEmergency(app *models.AppContext, update tgbotapi.Update) {
	chatID := update.Message.Chat.ID

	// Проверяем, является ли пользователь админом
	if !isAdmin(app, chatID) {
		msg := tgbotapi.NewMessage(chatID, config.MSG_NO_PERMISSION)
		app.Bot.Send(msg)
		return
	}

	// Получаем все активные аварийные заявки
	requests, err := getActiveEmergencyRequests(app)
	if err != nil {
		log.Printf("Ошибка получения заявок: %v", err)
		msg := tgbotapi.NewMessage(chatID, config.MSG_ERROR_OCCURRED)
		app.Bot.Send(msg)
		return
	}

	if len(requests) == 0 {
		msg := tgbotapi.NewMessage(chatID, "Активных аварийных заявок нет.")
		app.Bot.Send(msg)
		return
	}

	// Формируем сообщение со списком заявок
	message := "🚨 *Активные аварийные заявки:*\n\n"

	for i, req := range requests {
		// Определяем статус для отображения
		var statusText string
		switch req.Status {
		case config.EMERGENCY_STATUS_NEW:
			statusText = "🆕 Новая"
		case config.EMERGENCY_STATUS_CONFIRMED:
			statusText = "✅ Подтверждена"
		case config.EMERGENCY_STATUS_IN_PROGRESS:
			statusText = "🛠 В работе"
		default:
			statusText = req.Status
		}

		// Получаем имя пользователя
		var username string
		user, err := getUser(app, req.ChatID)
		if err == nil {
			username = user.Username
			if username == "" {
				username = fmt.Sprintf("Пользователь %d", req.ChatID)
			}
		} else {
			username = fmt.Sprintf("Пользователь %d", req.ChatID)
		}

		// Добавляем информацию о заявке
		message += fmt.Sprintf(
			"%d. ID: `%s`\n"+
				"Квартира: %d\n"+
				"Пользователь: %s\n"+
				"Дата: %s\n"+
				"Статус: %s\n"+
				"Приоритет: %d/5\n"+
				"Описание: %s\n",
			i+1,
			req.ID.Hex(),
			req.ApartmentNumber,
			username,
			req.Date.Format("02.01.2006 15:04"),
			statusText,
			req.Priority,
			utils.TruncateText(req.Description, 100), // Ограничиваем длину описания
		)

		// Добавляем контактный телефон, если указан
		if req.ContactPhone != "" {
			message += fmt.Sprintf("Телефон: %s\n", req.ContactPhone)
		}

		// Добавляем информацию об исполнителе, если есть
		if req.AssignedTo != "" {
			message += fmt.Sprintf("Исполнитель: %s\n", req.AssignedTo)
		}

		// Добавляем клавиатуру для управления только для первых 5 заявок
		// (из-за ограничений Telegram на число кнопок)
		if i < 5 {
			message += fmt.Sprintf("\nДействия: /e_confirm_%s /e_reject_%s /e_progress_%s\n",
				req.ID.Hex(), req.ID.Hex(), req.ID.Hex())
		}

		// Добавляем разделитель между заявками
		if i < len(requests)-1 {
			message += "\n-------------------\n\n"
		}
	}

	// Если заявок много, добавляем примечание о максимальном числе кнопок
	if len(requests) > 5 {
		message += "\n\nПоказаны кнопки управления только для первых 5 заявок. Для остальных используйте команды вручную."
	}

	// Разбиваем большое сообщение на части, если превышает максимальную длину
	if len(message) > 4000 {
		chunks := utils.SplitMessage(message, 4000)
		for _, chunk := range chunks {
			msg := tgbotapi.NewMessage(chatID, chunk)
			msg.ParseMode = "Markdown"
			app.Bot.Send(msg)
		}
	} else {
		msg := tgbotapi.NewMessage(chatID, message)
		msg.ParseMode = "Markdown"
		app.Bot.Send(msg)
	}
}

// Обработка создания описания аварийной заявки
func handleEmergencyDescription(app *models.AppContext, update tgbotapi.Update) {
	chatID := update.Message.Chat.ID
	description := update.Message.Text

	// Проверяем длину описания
	if len(description) < 10 {
		msg := tgbotapi.NewMessage(chatID, "Описание слишком короткое. Пожалуйста, предоставьте более подробную информацию.")
		app.Bot.Send(msg)
		return
	}

	// Получаем данные пользователя
	user, err := getUser(app, chatID)
	if err != nil {
		log.Printf("Ошибка получения данных пользователя: %v", err)
		msg := tgbotapi.NewMessage(chatID, config.MSG_ERROR_OCCURRED)
		app.Bot.Send(msg)
		return
	}

	// Меняем состояние на ожидание контактного телефона
	setUserState(app, chatID, "EMERGENCY_WAITING_PHONE")

	// Сохраняем описание во временное хранилище (можно использовать Redis или просто карту в памяти)
	// Сохраняем описание во временное хранилище
	app.UserStatesMutex.Lock()
	tempData := make(map[string]interface{})
	tempData["description"] = description
	tempData["apartment_number"] = user.ApartmentNumber // Используем user
	app.UserStates[chatID] = "EMERGENCY_WAITING_PHONE"
	app.UserStateData[chatID] = tempData
	app.UserStatesMutex.Unlock()

	// Запрашиваем контактный телефон
	msg := tgbotapi.NewMessage(chatID,
		"Укажите контактный телефон для связи с вами по данной заявке (или нажмите кнопку ниже):")

	// Создаем кнопку для отправки контакта
	contactButton := tgbotapi.NewKeyboardButtonContact("📞 Отправить мой номер")
	skipButton := tgbotapi.NewKeyboardButton("⏩ Пропустить")

	keyboard := tgbotapi.NewReplyKeyboard(
		tgbotapi.NewKeyboardButtonRow(contactButton),
		tgbotapi.NewKeyboardButtonRow(skipButton),
	)
	keyboard.OneTimeKeyboard = true
	msg.ReplyMarkup = keyboard

	app.Bot.Send(msg)
}

// Обработка ввода контактного телефона для аварийной заявки
func handleEmergencyPhone(app *models.AppContext, update tgbotapi.Update) {
	chatID := update.Message.Chat.ID

	var contactPhone string
	// Проверяем, отправил ли пользователь свой контакт через кнопку
	if update.Message.Contact != nil {
		contactPhone = update.Message.Contact.PhoneNumber
	} else if update.Message.Text == "⏩ Пропустить" {
		contactPhone = "" // Пользователь решил пропустить ввод телефона
	} else {
		// Пользователь ввел телефон текстом
		contactPhone = update.Message.Text
	}

	// Получаем данные пользователя
	_, err := getUser(app, chatID)
	if err != nil {
		log.Printf("Ошибка получения данных пользователя: %v", err)
		msg := tgbotapi.NewMessage(chatID, config.MSG_ERROR_OCCURRED)
		app.Bot.Send(msg)
		return
	}
	// Получаем сохраненное описание заявки
	app.UserStatesMutex.Lock()
	tempData, ok := app.UserStateData[chatID].(map[string]interface{})
	app.UserStatesMutex.Unlock()

	if !ok || tempData["description"] == nil {
		log.Printf("Ошибка получения временных данных для пользователя %d", chatID)
		msg := tgbotapi.NewMessage(chatID, config.MSG_ERROR_OCCURRED)
		app.Bot.Send(msg)
		return
	}

	//	description := tempData["description"].(string)

	// Запрашиваем приоритет заявки
	setUserState(app, chatID, "EMERGENCY_WAITING_PRIORITY")

	// Обновляем временные данные
	app.UserStatesMutex.Lock()
	tempData["contact_phone"] = contactPhone
	app.UserStateData[chatID] = tempData
	app.UserStatesMutex.Unlock()

	// Запрашиваем приоритет через кнопки
	msg := tgbotapi.NewMessage(chatID,
		"Укажите приоритет заявки:\n\n"+
			"1️⃣ - Низкий (незначительная проблема)\n"+
			"2️⃣ - Ниже среднего\n"+
			"3️⃣ - Средний (стандартная проблема)\n"+
			"4️⃣ - Высокий (требуется срочное внимание)\n"+
			"5️⃣ - Критический (авария, затопление, и т.д.)")

	keyboard := tgbotapi.NewReplyKeyboard(
		tgbotapi.NewKeyboardButtonRow(
			tgbotapi.NewKeyboardButton("1️⃣"),
			tgbotapi.NewKeyboardButton("2️⃣"),
			tgbotapi.NewKeyboardButton("3️⃣"),
			tgbotapi.NewKeyboardButton("4️⃣"),
			tgbotapi.NewKeyboardButton("5️⃣"),
		),
	)
	keyboard.OneTimeKeyboard = true
	msg.ReplyMarkup = keyboard

	app.Bot.Send(msg)
}

// Обработка выбора приоритета для аварийной заявки
func handleEmergencyPriority(app *models.AppContext, update tgbotapi.Update) {
	chatID := update.Message.Chat.ID
	priorityText := update.Message.Text

	// Преобразуем текст в числовой приоритет
	var priority int
	switch priorityText {
	case "1️⃣":
		priority = 1
	case "2️⃣":
		priority = 2
	case "3️⃣":
		priority = 3
	case "4️⃣":
		priority = 4
	case "5️⃣":
		priority = 5
	default:
		// Если пользователь ввел что-то другое, пытаемся преобразовать текст в число
		var err error
		priority, err = strconv.Atoi(strings.TrimSpace(priorityText))
		if err != nil || priority < 1 || priority > 5 {
			priority = 3 // Устанавливаем средний приоритет по умолчанию
		}
	}

	// Получаем данные пользователя
	user, err := getUser(app, chatID)
	if err != nil {
		log.Printf("Ошибка получения данных пользователя: %v", err)
		msg := tgbotapi.NewMessage(chatID, config.MSG_ERROR_OCCURRED)
		app.Bot.Send(msg)
		return
	}

	// Получаем сохраненные временные данные
	app.UserStatesMutex.Lock()
	tempData, ok := app.UserStateData[chatID].(map[string]interface{})
	app.UserStatesMutex.Unlock()

	if !ok || tempData["description"] == nil {
		log.Printf("Ошибка получения временных данных для пользователя %d", chatID)
		msg := tgbotapi.NewMessage(chatID, config.MSG_ERROR_OCCURRED)
		app.Bot.Send(msg)
		return
	}

	description := tempData["description"].(string)
	contactPhone, _ := tempData["contact_phone"].(string)

	// Устанавливаем массив фотографий пустым, так как в данной реализации мы их не запрашиваем
	photos := []string{}

	// Создаем аварийную заявку в базе данных
	id, err := createEmergencyRequest(app, user.ApartmentNumber, chatID, description, contactPhone, photos, priority)
	if err != nil {
		log.Printf("Ошибка создания аварийной заявки: %v", err)
		msg := tgbotapi.NewMessage(chatID, "Ошибка создания заявки: "+err.Error())
		app.Bot.Send(msg)
		return
	}

	// Очищаем временные данные
	app.UserStatesMutex.Lock()
	delete(app.UserStateData, chatID)
	app.UserStatesMutex.Unlock()

	// Сбрасываем состояние пользователя
	setUserState(app, chatID, config.STATE_WAITING_READINGS)

	// Отправляем подтверждение создания заявки
	msg := tgbotapi.NewMessage(chatID,
		fmt.Sprintf("✅ Аварийная заявка успешно создана!\n\n"+
			"ID заявки: `%s`\n"+
			"Приоритет: %d/5\n\n"+
			"Специалисты будут уведомлены о вашей проблеме.\n"+
			"Вы получите уведомление при изменении статуса заявки.",
			id.Hex(), priority))
	msg.ParseMode = "Markdown"
	app.Bot.Send(msg)

	// Возвращаем обычную клавиатуру
	msg = tgbotapi.NewMessage(chatID, "Вы можете проверить статус заявки командой /myemergency")
	msg.ReplyMarkup = tgbotapi.NewRemoveKeyboard(true)
	app.Bot.Send(msg)
}

// Обработка команд изменения статуса аварийной заявки (для админов)
func handleEmergencyStatusCommand(app *models.AppContext, update tgbotapi.Update, newStatus string) {
	chatID := update.Message.Chat.ID

	// Проверяем, является ли пользователь админом
	if !isAdmin(app, chatID) {
		msg := tgbotapi.NewMessage(chatID, config.MSG_NO_PERMISSION)
		app.Bot.Send(msg)
		return
	}

	// Извлекаем ID заявки из команды
	command := update.Message.Text
	parts := strings.Split(command, "_")
	if len(parts) != 3 {
		msg := tgbotapi.NewMessage(chatID, "Неверный формат команды. Используйте /e_action_id")
		app.Bot.Send(msg)
		return
	}

	idHex := parts[2]
	id, err := primitive.ObjectIDFromHex(idHex)
	if err != nil {
		msg := tgbotapi.NewMessage(chatID, "Неверный формат ID заявки.")
		app.Bot.Send(msg)
		return
	}

	// Устанавливаем имя администратора в качестве исполнителя
	assignedTo := update.Message.From.UserName
	if assignedTo == "" {
		assignedTo = fmt.Sprintf("Admin %d", chatID)
	}

	// Определяем текст примечания в зависимости от статуса
	var notes string
	switch newStatus {
	case config.EMERGENCY_STATUS_NEW:
		notes = "Заявка подтверждена администратором."
	case config.EMERGENCY_STATUS_IN_PROGRESS:
		notes = "Заявка взята в работу."
	case config.EMERGENCY_STATUS_REJECTED:
		notes = "Заявка отклонена администратором."
	case config.EMERGENCY_STATUS_COMPLETED:
		notes = "Заявка помечена как выполненная."
	}

	// Обновляем статус заявки
	err = updateEmergencyStatus(app, id, newStatus, assignedTo, notes)
	if err != nil {
		log.Printf("Ошибка обновления статуса заявки: %v", err)
		msg := tgbotapi.NewMessage(chatID, "Ошибка: "+err.Error())
		app.Bot.Send(msg)
		return
	}

	// Отправляем подтверждение
	msg := tgbotapi.NewMessage(chatID, fmt.Sprintf("Статус заявки %s успешно изменен на %s", idHex, newStatus))
	app.Bot.Send(msg)
}

// Обработка нажатий на инлайн-кнопки для аварийных заявок
func handleEmergencyCallback(app *models.AppContext, update tgbotapi.Update) {
	callback := update.CallbackQuery
	data := callback.Data

	// Проверяем, что это коллбэк для аварийной заявки
	if !strings.HasPrefix(data, "emergency_") {
		return
	}

	// Извлекаем действие и ID заявки
	parts := strings.Split(data, "_")
	if len(parts) != 3 {
		return
	}

	action := parts[1]
	idHex := parts[2]

	// Преобразуем строковый ID в ObjectID
	id, err := primitive.ObjectIDFromHex(idHex)
	if err != nil {
		return
	}

	// Определяем новый статус в зависимости от действия
	var newStatus string
	switch action {
	case "confirm":
		newStatus = config.EMERGENCY_STATUS_CONFIRMED
	case "reject":
		newStatus = config.EMERGENCY_STATUS_REJECTED
	case "progress":
		newStatus = config.EMERGENCY_STATUS_IN_PROGRESS
	case "complete":
		newStatus = config.EMERGENCY_STATUS_COMPLETED
	default:
		return
	}

	// Устанавливаем имя администратора в качестве исполнителя
	assignedTo := callback.From.UserName
	if assignedTo == "" {
		assignedTo = fmt.Sprintf("Admin %d", callback.From.ID)
	}

	// Определяем текст примечания в зависимости от статуса
	var notes string
	switch newStatus {
	case config.EMERGENCY_STATUS_CONFIRMED:
		notes = "Заявка подтверждена через бот."
	case config.EMERGENCY_STATUS_IN_PROGRESS:
		notes = "Заявка взята в работу через бот."
	case config.EMERGENCY_STATUS_REJECTED:
		notes = "Заявка отклонена через бот."
	case config.EMERGENCY_STATUS_COMPLETED:
		notes = "Заявка помечена как выполненная через бот."
	}

	// Обновляем статус заявки
	err = updateEmergencyStatus(app, id, newStatus, assignedTo, notes)

	// Отправляем уведомление о получении коллбэка
	app.Bot.Request(tgbotapi.NewCallback(callback.ID, ""))

	// Обновляем исходное сообщение
	var responseText string
	if err != nil {
		responseText = "❌ Ошибка: " + err.Error()
	} else {
		responseText = fmt.Sprintf("✅ Статус изменен на: %s\nИсполнитель: %s", newStatus, assignedTo)
	}

	// Редактируем сообщение, удаляя кнопки и добавляя информацию о действии
	edit := tgbotapi.NewEditMessageText(
		callback.Message.Chat.ID,
		callback.Message.MessageID,
		callback.Message.Text+"\n\n"+responseText)
	edit.ReplyMarkup = nil
	app.Bot.Send(edit)
}

// Обработка регистрации квартиры
func handleApartmentRegistration(app *models.AppContext, update tgbotapi.Update) {
	chatID := update.Message.Chat.ID
	messageText := update.Message.Text

	// Проверяем формат номера квартиры
	apartmentNumberStr := strings.TrimSpace(messageText)
	apartmentNumber, err := strconv.Atoi(apartmentNumberStr)
	if err != nil || apartmentNumber < config.MIN_APARTMENT_NUMBER || apartmentNumber > config.MAX_APARTMENT_NUMBER {
		msg := tgbotapi.NewMessage(chatID, fmt.Sprintf(config.MSG_INVALID_APARTMENT, config.MIN_APARTMENT_NUMBER, config.MAX_APARTMENT_NUMBER))
		app.Bot.Send(msg)
		return
	}

	// Создаем нового пользователя
	err = registerUser(app, chatID, update.Message.From.UserName, apartmentNumber)
	if err != nil {
		log.Printf("Ошибка регистрации: %v", err)

		// Проверяем, не связана ли ошибка с уже зарегистрированной квартирой
		if strings.Contains(err.Error(), "уже зарегистрирована") {
			msg := tgbotapi.NewMessage(chatID, fmt.Sprintf("Квартира %d уже зарегистрирована в системе. Пожалуйста, укажите другой номер квартиры:", apartmentNumber))
			app.Bot.Send(msg)
			return
		}

		msg := tgbotapi.NewMessage(chatID, "Ошибка: "+err.Error())
		app.Bot.Send(msg)
		return
	}

	// Отправляем сообщение об успешной регистрации
	msg := tgbotapi.NewMessage(chatID, fmt.Sprintf(config.MSG_REGISTRATION_SUCCESS, apartmentNumber))
	app.Bot.Send(msg)

	// Меняем состояние на ожидание показаний
	setUserState(app, chatID, config.STATE_WAITING_READINGS)

	// Просим пользователя ввести показания
	msg = tgbotapi.NewMessage(chatID, config.MSG_START_REGISTERED)
	app.Bot.Send(msg)
}

// Обработка текстовых сообщений
// Обработка текстовых сообщений
func handleMessage(app *models.AppContext, update tgbotapi.Update) {
	// Игнорируем пустые сообщения или команды
	if update.Message == nil || update.Message.Text == "" || strings.HasPrefix(update.Message.Text, "/") {
		return
	}

	chatID := update.Message.Chat.ID

	// Проверка режима обслуживания
	if app.Config.MaintenanceMode && !isAdmin(app, chatID) {
		msg := tgbotapi.NewMessage(chatID, app.Config.MaintenanceMessage)
		app.Bot.Send(msg)
		return
	}

	// Получаем текущее состояние пользователя
	state := getUserState(app, chatID)

	// Обрабатываем сообщение в зависимости от состояния
	switch state {
	case "EMERGENCY_WAITING_DESCRIPTION":
		// Обрабатываем ввод описания аварийной ситуации
		handleEmergencyDescription(app, update)

	case "EMERGENCY_WAITING_PHONE":
		// Обрабатываем ввод контактного телефона
		handleEmergencyPhone(app, update)

	case "EMERGENCY_WAITING_PRIORITY":
		// Обрабатываем выбор приоритета
		handleEmergencyPriority(app, update)
	case config.STATE_NEW:
		// Предлагаем новому пользователю начать процесс регистрации
		setUserState(app, chatID, config.STATE_WAITING_APARTMENT)
		msg := tgbotapi.NewMessage(
			chatID,
			fmt.Sprintf(config.MSG_START_NOT_REGISTERED, config.MIN_APARTMENT_NUMBER, config.MAX_APARTMENT_NUMBER),
		)
		app.Bot.Send(msg)

	case config.STATE_WAITING_APARTMENT:
		// Обрабатываем ввод номера квартиры
		handleApartmentRegistration(app, update)

	case config.STATE_WAITING_READINGS:
		// Обрабатываем ввод показаний
		handleReadingsInput(app, update)

	case config.STATE_CONFIRMING_READINGS:
		// Обрабатываем подтверждение показаний (для случаев с большой разницей)
		// Этот случай обычно обрабатывается через CallbackQuery, но можно добавить текстовое подтверждение
		msg := tgbotapi.NewMessage(chatID, "Пожалуйста, используйте кнопки подтверждения ниже сообщения.")
		app.Bot.Send(msg)

	case config.STATE_ADMIN_WAITING_NEWS:
		// Только для админов - обработка ввода текста новости
		if isAdmin(app, chatID) {
			handleNewsInput(app, update)
		} else {
			setUserState(app, chatID, config.STATE_WAITING_READINGS)
			msg := tgbotapi.NewMessage(chatID, config.MSG_NO_PERMISSION)
			app.Bot.Send(msg)
		}

	default:
		// Неизвестное состояние - сбрасываем на ожидание показаний
		log.Printf("Неизвестное состояние пользователя %d: %s", chatID, state)
		setUserState(app, chatID, config.STATE_WAITING_READINGS)
		msg := tgbotapi.NewMessage(chatID, config.MSG_START_REGISTERED)
		app.Bot.Send(msg)
	}
}

// Общая функция обработки всех команд
func handleCommand(app *models.AppContext, update tgbotapi.Update) {
	command := update.Message.Command()

	switch command {
	case "start":
		handleStart(app, update)
	case "history":
		handleHistory(app, update)
	case "stats":
		handleStats(app, update)
	case "export":
		handleExport(app, update)
	case "addnews":
		handleAddNews(app, update)
	case "sendnews":
		handleSendNews(app, update)
	case "remind":
		handleRemind(app, update)
	case "backup":
		handleBackup(app, update)
	case "maintenance":
		handleMaintenance(app, update)
	case "setreadings":
		handleSetReadings(app, update)
	case "emergency":
		handleEmergency(app, update)
	case "myemergency":
		handleMyEmergency(app, update)
	case "allemergency":
		handleAllEmergency(app, update)
	// Кейсы для команд изменения статуса
	case "e_confirm", "econfirim":
		parts := strings.Split(update.Message.Text, "_")
		if len(parts) >= 3 {
			handleEmergencyStatusCommand(app, update, config.EMERGENCY_STATUS_CONFIRMED)
		} else {
			msg := tgbotapi.NewMessage(update.Message.Chat.ID, "Неверный формат команды. Используйте /e_confirm_id")
			app.Bot.Send(msg)
		}
	case "e_reject", "ereject":
		parts := strings.Split(update.Message.Text, "_")
		if len(parts) >= 3 {
			handleEmergencyStatusCommand(app, update, config.EMERGENCY_STATUS_REJECTED)
		} else {
			msg := tgbotapi.NewMessage(update.Message.Chat.ID, "Неверный формат команды. Используйте /e_reject_id")
			app.Bot.Send(msg)
		}
	case "e_progress", "eprogress":
		parts := strings.Split(update.Message.Text, "_")
		if len(parts) >= 3 {
			handleEmergencyStatusCommand(app, update, config.EMERGENCY_STATUS_IN_PROGRESS)
		} else {
			msg := tgbotapi.NewMessage(update.Message.Chat.ID, "Неверный формат команды. Используйте /e_progress_id")
			app.Bot.Send(msg)
		}
	case "e_complete", "ecomplete":
		parts := strings.Split(update.Message.Text, "_")
		if len(parts) >= 3 {
			handleEmergencyStatusCommand(app, update, config.EMERGENCY_STATUS_COMPLETED)
		} else {
			msg := tgbotapi.NewMessage(update.Message.Chat.ID, "Неверный формат команды. Используйте /e_complete_id")
			app.Bot.Send(msg)
		}
	default:
		// Неизвестная команда
		msg := tgbotapi.NewMessage(update.Message.Chat.ID, "Неизвестная команда. Введите /start для начала работы.")
		app.Bot.Send(msg)
	}
}

// submitReadingsHandler обрабатывает ввод показаний через API
func submitReadingsHandler(app *models.AppContext) gin.HandlerFunc {
	return func(c *gin.Context) {
		var request models.ReadingSubmissionRequest
		if err := c.ShouldBindJSON(&request); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Неверный формат запроса: " + err.Error()})
			return
		}

		// Проверка валидности показаний
		if !validateReading(request.Cold) || !validateReading(request.Hot) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Недопустимые значения показаний"})
			return
		}

		// Проверка существования квартиры
		ctx, cancel := context.WithTimeout(context.Background(), config.DATABASE_TIMEOUT)
		defer cancel()

		var user models.User
		err := app.UsersCollection.FindOne(ctx, bson.M{"apartment_number": request.ApartmentNumber}).Decode(&user)
		if err != nil {
			if err == mongo.ErrNoDocuments {
				c.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("Квартира %d не зарегистрирована", request.ApartmentNumber)})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Ошибка базы данных: " + err.Error()})
			return
		}

		// Если не указан ChatID, используем ChatID пользователя из базы
		chatID := request.ChatID
		if chatID == 0 {
			chatID = user.ChatID
		}

		// Проверка предыдущих показаний
		lastCold, lastHot, _, err := getLastReadings(app, request.ApartmentNumber)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Ошибка получения последних показаний: " + err.Error()})
			return
		}

		// Проверка, что новые показания не меньше предыдущих
		if lastCold > 0 && request.Cold < lastCold {
			c.JSON(http.StatusBadRequest, gin.H{
				"error": fmt.Sprintf("Новое показание холодной воды (%.2f) должно быть больше или равно предыдущему (%.2f)",
					request.Cold, lastCold),
			})
			return
		}

		if lastHot > 0 && request.Hot < lastHot {
			c.JSON(http.StatusBadRequest, gin.H{
				"error": fmt.Sprintf("Новое показание горячей воды (%.2f) должно быть больше или равно предыдущему (%.2f)",
					request.Hot, lastHot),
			})
			return
		}

		// Проверка большой разницы в показаниях
		coldDiff := request.Cold - lastCold
		hotDiff := request.Hot - lastHot

		if lastCold > 0 && coldDiff > app.Config.MaxColdWaterIncrease {
			c.JSON(http.StatusBadRequest, gin.H{
				"error":                fmt.Sprintf("Разница в показаниях холодной воды слишком велика (%.2f)", coldDiff),
				"require_confirmation": true,
				"type":                 "cold_water",
				"previous":             lastCold,
				"new":                  request.Cold,
			})
			return
		}

		if lastHot > 0 && hotDiff > app.Config.MaxHotWaterIncrease {
			c.JSON(http.StatusBadRequest, gin.H{
				"error":                fmt.Sprintf("Разница в показаниях горячей воды слишком велика (%.2f)", hotDiff),
				"require_confirmation": true,
				"type":                 "hot_water",
				"previous":             lastHot,
				"new":                  request.Hot,
			})
			return
		}

		// Установка даты, если не указана
		submitDate := request.Date
		if submitDate.IsZero() {
			submitDate = time.Now()
		}

		// Сохранение показаний
		reading := models.Reading{
			ApartmentNumber: request.ApartmentNumber,
			ChatID:          chatID,
			Date:            submitDate,
			Cold:            request.Cold,
			Hot:             request.Hot,
			Verified:        app.Config.AutoConfirmReadings,
		}

		_, err = app.ReadingsCollection.InsertOne(ctx, reading)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Ошибка сохранения показаний: " + err.Error()})
			return
		}

		// Логирование
		log.Printf("API: Показания сохранены: Квартира=%d, Холодная=%.2f, Горячая=%.2f",
			request.ApartmentNumber, request.Cold, request.Hot)

		c.JSON(http.StatusOK, gin.H{
			"success":   true,
			"apartment": request.ApartmentNumber,
			"cold":      request.Cold,
			"hot":       request.Hot,
			"date":      submitDate.Format(time.RFC3339),
		})
	}
}

// adminSubmitReadingsHandler позволяет администраторам вводить показания для любой квартиры
func adminSubmitReadingsHandler(app *models.AppContext) gin.HandlerFunc {
	return func(c *gin.Context) {
		var request models.AdminReadingSubmissionRequest
		if err := c.ShouldBindJSON(&request); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Неверный формат запроса: " + err.Error()})
			return
		}

		// Проверка валидности показаний
		if !validateReading(request.Cold) || !validateReading(request.Hot) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Недопустимые значения показаний"})
			return
		}

		// Проверка существования квартиры
		ctx, cancel := context.WithTimeout(context.Background(), config.DATABASE_TIMEOUT)
		defer cancel()

		var user models.User
		err := app.UsersCollection.FindOne(ctx, bson.M{"apartment_number": request.ApartmentNumber}).Decode(&user)
		if err != nil {
			if err == mongo.ErrNoDocuments {
				c.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("Квартира %d не зарегистрирована", request.ApartmentNumber)})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Ошибка базы данных: " + err.Error()})
			return
		}

		// Проверка предыдущих показаний, но только для информации
		lastCold, lastHot, _, _ := getLastReadings(app, request.ApartmentNumber)

		// Получение имени админа
		adminUsername := request.AdminUsername
		if adminUsername == "" {
			adminUsername = "API"
		}

		// Установка даты, если не указана
		submitDate := request.Date
		if submitDate.IsZero() {
			submitDate = time.Now()
		}

		// Сохранение показаний с отметкой администратора
		reading := models.Reading{
			ApartmentNumber: request.ApartmentNumber,
			ChatID:          user.ChatID,
			Date:            submitDate,
			Cold:            request.Cold,
			Hot:             request.Hot,
			Verified:        true, // показания от админа всегда подтверждены
			VerifiedBy:      adminUsername,
		}

		_, err = app.ReadingsCollection.InsertOne(ctx, reading)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Ошибка сохранения показаний: " + err.Error()})
			return
		}

		// Логирование
		log.Printf("API: Админ %s ввел показания: Квартира=%d, Холодная=%.2f, Горячая=%.2f",
			adminUsername, request.ApartmentNumber, request.Cold, request.Hot)

		// Отправляем уведомление пользователю через Telegram, если бот активен
		if app.Bot != nil {
			notificationText := fmt.Sprintf(
				"Администратор внес показания для вашей квартиры:\nХолодная вода: %.2f\nГорячая вода: %.2f\nДата: %s",
				request.Cold, request.Hot, submitDate.Format("2006-01-02 15:04:05"))

			msg := tgbotapi.NewMessage(user.ChatID, notificationText)
			go app.Bot.Send(msg) // отправляем асинхронно
		}

		c.JSON(http.StatusOK, gin.H{
			"success":       true,
			"apartment":     request.ApartmentNumber,
			"cold":          request.Cold,
			"hot":           request.Hot,
			"previous_cold": lastCold,
			"previous_hot":  lastHot,
			"date":          submitDate.Format(time.RFC3339),
			"admin":         adminUsername,
		})
	}
}

// confirmReadingsHandler позволяет явно подтвердить показания с большой разницей
func confirmReadingsHandler(app *models.AppContext) gin.HandlerFunc {
	return func(c *gin.Context) {
		var request models.ReadingSubmissionRequest
		if err := c.ShouldBindJSON(&request); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Неверный формат запроса: " + err.Error()})
			return
		}

		// Добавьте этот обработчик в setupAPI
		// api.POST("/readings/confirm", confirmReadingsHandler(app))

		// Здесь код аналогичен submitReadingsHandler, но без проверок на большую разницу
		// Показания сохраняются принудительно с пометкой "confirmed"

		ctx, cancel := context.WithTimeout(context.Background(), config.DATABASE_TIMEOUT)
		defer cancel()

		// Находим пользователя по номеру квартиры
		var user models.User
		err := app.UsersCollection.FindOne(ctx, bson.M{"apartment_number": request.ApartmentNumber}).Decode(&user)
		if err != nil {
			if err == mongo.ErrNoDocuments {
				c.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("Квартира %d не зарегистрирована", request.ApartmentNumber)})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Ошибка базы данных: " + err.Error()})
			return
		}

		// Если не указан ChatID, используем ChatID пользователя из базы
		chatID := request.ChatID
		if chatID == 0 {
			chatID = user.ChatID
		}

		// Установка даты, если не указана
		submitDate := request.Date
		if submitDate.IsZero() {
			submitDate = time.Now()
		}

		// Сохранение показаний с пометкой о подтверждении
		reading := models.Reading{
			ApartmentNumber: request.ApartmentNumber,
			ChatID:          chatID,
			Date:            submitDate,
			Cold:            request.Cold,
			Hot:             request.Hot,
			Verified:        true, // показания подтверждены явно
			VerifiedBy:      "API_confirmed",
		}

		_, err = app.ReadingsCollection.InsertOne(ctx, reading)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Ошибка сохранения показаний: " + err.Error()})
			return
		}

		// Логирование
		log.Printf("API: Показания с подтверждением сохранены: Квартира=%d, Холодная=%.2f, Горячая=%.2f",
			request.ApartmentNumber, request.Cold, request.Hot)

		c.JSON(http.StatusOK, gin.H{
			"success":   true,
			"confirmed": true,
			"apartment": request.ApartmentNumber,
			"cold":      request.Cold,
			"hot":       request.Hot,
			"date":      submitDate.Format(time.RFC3339),
		})
	}
}

// Handling the /setreadings command (admin only)
func handleSetReadings(app *models.AppContext, update tgbotapi.Update) {
	chatID := update.Message.Chat.ID

	// Verify the user is an admin
	if !isAdmin(app, chatID) {
		msg := tgbotapi.NewMessage(chatID, config.MSG_NO_PERMISSION)
		app.Bot.Send(msg)
		return
	}

	// Parse command arguments
	args := strings.TrimSpace(update.Message.CommandArguments())
	parts := strings.Fields(args)

	// Check format
	if len(parts) != 3 {
		msg := tgbotapi.NewMessage(chatID, "Использование: /setreadings <номер_квартиры> <холодная> <горячая>")
		app.Bot.Send(msg)
		return
	}

	// Parse apartment number
	apartmentNumber, err := strconv.Atoi(parts[0])
	if err != nil || apartmentNumber < config.MIN_APARTMENT_NUMBER || apartmentNumber > config.MAX_APARTMENT_NUMBER {
		msg := tgbotapi.NewMessage(chatID, fmt.Sprintf("Номер квартиры должен быть числом от %d до %d.",
			config.MIN_APARTMENT_NUMBER, config.MAX_APARTMENT_NUMBER))
		app.Bot.Send(msg)
		return
	}

	// Parse readings
	cold, err := strconv.ParseFloat(parts[1], 64)
	if err != nil || cold <= 0 {
		msg := tgbotapi.NewMessage(chatID, config.MSG_VALUE_MUST_BE_POSITIVE)
		app.Bot.Send(msg)
		return
	}

	hot, err := strconv.ParseFloat(parts[2], 64)
	if err != nil || hot <= 0 {
		msg := tgbotapi.NewMessage(chatID, config.MSG_VALUE_MUST_BE_POSITIVE)
		app.Bot.Send(msg)
		return
	}

	// Verify the apartment exists in the database
	ctx, cancel := context.WithTimeout(context.Background(), config.DATABASE_TIMEOUT)
	defer cancel()

	var user models.User
	err = app.UsersCollection.FindOne(ctx, bson.M{"apartment_number": apartmentNumber}).Decode(&user)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			msg := tgbotapi.NewMessage(chatID, fmt.Sprintf("Квартира %d не зарегистрирована в системе.", apartmentNumber))
			app.Bot.Send(msg)
			return
		}
		log.Printf("Ошибка проверки квартиры: %v", err)
		msg := tgbotapi.NewMessage(chatID, config.MSG_ERROR_OCCURRED)
		app.Bot.Send(msg)
		return
	}

	// Check if readings are valid compared to previous readings
	lastCold, lastHot, _, err := getLastReadings(app, apartmentNumber)
	if err != nil {
		log.Printf("Ошибка получения последних показаний: %v", err)
		msg := tgbotapi.NewMessage(chatID, config.MSG_ERROR_OCCURRED)
		app.Bot.Send(msg)
		return
	}

	// Check readings are not decreasing
	if lastCold > 0 && cold < lastCold {
		msg := tgbotapi.NewMessage(chatID, fmt.Sprintf(config.MSG_COLD_DECREASED, cold, lastCold))
		app.Bot.Send(msg)
		return
	}

	if lastHot > 0 && hot < lastHot {
		msg := tgbotapi.NewMessage(chatID, fmt.Sprintf(config.MSG_HOT_DECREASED, hot, lastHot))
		app.Bot.Send(msg)
		return
	}

	// Save the readings with admin mark
	reading := models.Reading{
		ApartmentNumber: apartmentNumber,
		ChatID:          user.ChatID, // Using the apartment owner's ChatID
		Date:            time.Now(),
		Cold:            cold,
		Hot:             hot,
		Verified:        true, // Admin-entered readings are automatically verified
		VerifiedBy:      update.Message.From.UserName,
	}

	_, err = app.ReadingsCollection.InsertOne(ctx, reading)
	if err != nil {
		log.Printf("Ошибка сохранения показаний: %v", err)
		msg := tgbotapi.NewMessage(chatID, "Ошибка сохранения показаний: "+err.Error())
		app.Bot.Send(msg)
		return
	}

	// Log the admin action
	log.Printf("Администратор %s ввел показания для квартиры %d: Холодная=%.2f, Горячая=%.2f",
		update.Message.From.UserName, apartmentNumber, cold, hot)

	// Send confirmation message
	msg := tgbotapi.NewMessage(chatID, fmt.Sprintf(
		"✅ Показания для квартиры %d успешно сохранены:\nХолодная вода: %.2f\nГорячая вода: %.2f\nДата: %s",
		apartmentNumber, cold, hot, time.Now().Format("2006-01-02 15:04:05")))
	app.Bot.Send(msg)

	// Optionally notify the apartment owner
	notifyMsg := tgbotapi.NewMessage(user.ChatID, fmt.Sprintf(
		"Администратор %s внес показания для вашей квартиры:\nХолодная вода: %.2f\nГорячая вода: %.2f\nДата: %s",
		update.Message.From.UserName, cold, hot, time.Now().Format("2006-01-02 15:04:05")))

	// Send notification to apartment owner if they're not the same as admin
	if user.ChatID != chatID {
		app.Bot.Send(notifyMsg)
	}
}

// Защита от спама и флуда
func rateLimiter() func(int64) bool {
	// Карта для хранения времени последнего сообщения для каждого пользователя
	userLastMessage := make(map[int64]time.Time)
	// Мьютекс для безопасного доступа к карте из разных горутин
	mu := &sync.Mutex{}

	return func(chatID int64) bool {
		mu.Lock()
		defer mu.Unlock()

		now := time.Now()
		lastTime, exists := userLastMessage[chatID]

		// Если пользователь отсутствует или прошло достаточно времени
		if !exists || now.Sub(lastTime) > 1*time.Second {
			userLastMessage[chatID] = now
			return true
		}

		return false
	}
}

// Инициализация приложения
func main() {
	app, err := initApp()
	if err != nil {
		log.Fatalf("Ошибка инициализации: %v", err)
	}
	defer app.MongoClient.Disconnect(context.Background())

	// Настраиваем API
	setupAPI(app)

	// Запускаем HTTP сервер в отдельной горутине
	go startHTTPServer(app)

	defer app.MongoClient.Disconnect(context.Background())

	log.Printf("Бот запущен: @%s", app.Bot.Self.UserName)

	// Запускаем периодические задачи
	startScheduledTasks(app)

	// Создаем лимитер для защиты от спама
	canProcess := rateLimiter()
	// Пример для Go с использованием библиотеки telegram-bot-api
	// Добавьте эту строку перед запуском основного цикла получения обновлений
	_, err = app.Bot.Request(tgbotapi.DeleteWebhookConfig{})
	if err != nil {
		log.Panic(err)
	}

	// Настройка получения обновлений
	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60

	updates := app.Bot.GetUpdatesChan(u)

	// Обработка входящих сообщений
	for update := range updates {
		// Обработка коллбэков от инлайн-кнопок
		if update.CallbackQuery != nil {
			chatID := update.CallbackQuery.Message.Chat.ID

			// Проверка режима обслуживания
			if app.Config.MaintenanceMode && !isAdmin(app, chatID) {
				// В режиме обслуживания просто подтверждаем коллбэк
				app.Bot.Request(tgbotapi.NewCallback(update.CallbackQuery.ID, ""))
				continue
			}

			// Проверка частоты запросов
			if !canProcess(chatID) {
				// Слишком много запросов, просто подтверждаем коллбэк
				app.Bot.Request(tgbotapi.NewCallback(update.CallbackQuery.ID, "Слишком много запросов"))
				continue
			}

			// Обрабатываем коллбэк в отдельной горутине
			go func(u tgbotapi.Update) {
				defer func() {
					if r := recover(); r != nil {
						log.Printf("Паника при обработке коллбэка: %v", r)
					}
				}()

				handleCallbackQuery(app, u)
			}(update)

			continue
		}

		// Проверяем наличие сообщения
		if update.Message == nil {
			continue
		}

		chatID := update.Message.Chat.ID

		// Ограничение частоты запросов
		if !canProcess(chatID) {
			msg := tgbotapi.NewMessage(chatID, config.MSG_LIMIT_EXCEEDED)
			app.Bot.Send(msg)
			continue
		}

		// Проверка длины сообщения
		if len(update.Message.Text) > config.MAX_MESSAGE_LENGTH {
			msg := tgbotapi.NewMessage(chatID, config.MSG_DATA_TOO_LARGE)
			app.Bot.Send(msg)
			continue
		}

		// Логгирование входящих сообщений
		log.Printf("[%s] %s", update.Message.From.UserName, update.Message.Text)

		// Обработка команд или сообщений в отдельной горутине
		go func(u tgbotapi.Update) {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("Паника при обработке сообщения: %v", r)
					msg := tgbotapi.NewMessage(u.Message.Chat.ID, config.MSG_ERROR_OCCURRED)
					app.Bot.Send(msg)
				}
			}()

			if u.Message.IsCommand() {
				handleCommand(app, u)
			} else {
				handleMessage(app, u)
			}
		}(update)
	}
}
