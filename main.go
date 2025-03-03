package main

import (
	"context"
	"encoding/csv"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/joho/godotenv"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readpref"
)

// Константы для настройки бота
const (
	MIN_APARTMENT_NUMBER    = 129              // Минимальный номер квартиры
	MAX_APARTMENT_NUMBER    = 255              // Максимальный номер квартиры
	MAX_MESSAGE_LENGTH      = 4096             // Максимальная длина сообщения
	DATABASE_TIMEOUT        = 10 * time.Second // Таймаут для операций с базой данных
	LOG_FOLDER              = "logs"           // Папка для логов
	BACKUP_FOLDER           = "backups"        // Папка для резервных копий
	MAX_COLD_WATER_INCREASE = 15.0             // Максимальное увеличение показаний холодной воды
	MAX_HOT_WATER_INCREASE  = 15.0             // Максимальное увеличение показаний горячей воды
	MAX_WATER_VALUE         = 100000.0         // Максимальное значение показаний счетчика
)

// Константы состояний пользователя
const (
	STATE_NEW                 = "NEW"                 // Новый пользователь
	STATE_WAITING_APARTMENT   = "WAITING_APARTMENT"   // Ожидание ввода номера квартиры
	STATE_WAITING_READINGS    = "WAITING_READINGS"    // Ожидание ввода показаний
	STATE_CONFIRMING_READINGS = "CONFIRMING_READINGS" // Подтверждение показаний при большой разнице
	STATE_ADMIN_WAITING_NEWS  = "ADMIN_WAITING_NEWS"  // Админ создаёт новость
)

// Структуры данных
type User struct {
	ID              primitive.ObjectID `bson:"_id,omitempty"`
	ChatID          int64              `bson:"chat_id"`
	Username        string             `bson:"username"`
	ApartmentNumber int                `bson:"apartment_number"`
	RegisteredAt    time.Time          `bson:"registered_at"`
	LastActive      time.Time          `bson:"last_active"`
}

type Reading struct {
	ID              primitive.ObjectID `bson:"_id,omitempty"`
	ApartmentNumber int                `bson:"apartment_number"`
	ChatID          int64              `bson:"chat_id"`
	Date            time.Time          `bson:"date"`
	Cold            float64            `bson:"cold"`
	Hot             float64            `bson:"hot"`
	Verified        bool               `bson:"verified"`
	VerifiedBy      string             `bson:"verified_by,omitempty"`
}

type News struct {
	ID        primitive.ObjectID `bson:"_id,omitempty"`
	Title     string             `bson:"title"`
	Content   string             `bson:"content"`
	Date      time.Time          `bson:"date"`
	CreatedBy string             `bson:"created_by"`
	Sent      bool               `bson:"sent"`
}

type Admin struct {
	ChatID    int64     `bson:"chat_id"`
	Username  string    `bson:"username"`
	Role      string    `bson:"role"` // superadmin, admin, moderator
	CreatedAt time.Time `bson:"created_at"`
}

type Config struct {
	AdminIDs             []int64 `bson:"admin_ids"`
	ReadingReminderDay   int     `bson:"reading_reminder_day"`
	AllowReadingsFrom    int     `bson:"allow_readings_from"`
	AllowReadingsTo      int     `bson:"allow_readings_to"`
	MaxColdWaterIncrease float64 `bson:"max_cold_water_increase"`
	MaxHotWaterIncrease  float64 `bson:"max_hot_water_increase"`
	AutoConfirmReadings  bool    `bson:"auto_confirm_readings"`
	BackupIntervalHours  int     `bson:"backup_interval_hours"`
	MaintenanceMode      bool    `bson:"maintenance_mode"`
	MaintenanceMessage   string  `bson:"maintenance_message"`
}

// Контекст приложения
type AppContext struct {
	Bot                *tgbotapi.BotAPI
	MongoClient        *mongo.Client
	UsersCollection    *mongo.Collection
	ReadingsCollection *mongo.Collection
	NewsCollection     *mongo.Collection
	AdminsCollection   *mongo.Collection
	ConfigCollection   *mongo.Collection
	Config             Config
	UserStates         map[int64]string // Карта состояний пользователей
	UserStatesMutex    *sync.Mutex      // Мьютекс для безопасного доступа к карте
}

// Константы для сообщений
const (
	MSG_START_NOT_REGISTERED   = "Вы не зарегистрированы. Введите номер своей квартиры (%d-%d):"
	MSG_START_REGISTERED       = "Введите показания счетчиков (холодная и горячая вода) через пробел:"
	MSG_REGISTRATION_SUCCESS   = "Вы успешно зарегистрированы как житель квартиры %d."
	MSG_INVALID_APARTMENT      = "Номер квартиры должен быть числом от %d до %d. Пожалуйста, повторите ввод:"
	MSG_ERROR_OCCURRED         = "Произошла ошибка. Пожалуйста, повторите попытку позже."
	MSG_NO_READINGS            = "У вас нет записанных показаний."
	MSG_READINGS_SUCCESS       = "Показания успешно приняты:\nХолодная вода: %.2f\nГорячая вода: %.2f\nДата приема: %s"
	MSG_COLD_DECREASED         = "Новое показание холодной воды (%.2f) должно быть больше или равно предыдущему (%.2f)."
	MSG_HOT_DECREASED          = "Новое показание горячей воды (%.2f) должно быть больше или равно предыдущему (%.2f)."
	MSG_LARGE_DIFFERENCE_COLD  = "Разница в показаниях холодной воды слишком велика (%.2f). Пожалуйста, подтвердите или исправьте данные:"
	MSG_LARGE_DIFFERENCE_HOT   = "Разница в показаниях горячей воды слишком велика (%.2f). Пожалуйста, подтвердите или исправьте данные:"
	MSG_HISTORY_TITLE          = "История показаний:\n"
	MSG_HISTORY_ENTRY          = "%d. Холодная: %.2f, Горячая: %.2f, Дата: %s\n"
	MSG_NO_PERMISSION          = "У вас нет прав для выполнения этой команды."
	MSG_NEWS_ADDED             = "Новость успешно добавлена."
	MSG_NEWS_SEND_SUCCESS      = "Новости успешно отправлены всем пользователям."
	MSG_REMINDERS_SENT         = "Напоминания успешно отправлены."
	MSG_ADDNEWS_USAGE          = "Использование: /addnews <заголовок> <текст>"
	MSG_INVALID_FORMAT         = "Неверный формат. Введите два числа через пробел (холодная и горячая вода):"
	MSG_VALUE_MUST_BE_POSITIVE = "Значение должно быть числом больше 0."
	MSG_REMINDER               = "⚠️ Не забудьте передать показания счетчиков воды!"
	MSG_MAINTENANCE_MODE       = "Бот находится в режиме обслуживания. Пожалуйста, попробуйте позже."
	MSG_READINGS_CONFIRMED     = "Показания подтверждены!"
	MSG_EXPORT_READY           = "Экспорт данных готов. Вы можете скачать файл."
	MSG_STATISTICS             = "Статистика потребления воды (квартира %d):\n\nХолодная вода:\n- Среднее: %.2f м³/месяц\n- Максимум: %.2f м³ (%s)\n- Минимум: %.2f м³ (%s)\n\nГорячая вода:\n- Среднее: %.2f м³/месяц\n- Максимум: %.2f м³ (%s)\n- Минимум: %.2f м³ (%s)"
	MSG_BACKUP_COMPLETED       = "Резервное копирование завершено успешно."
	MSG_DATA_TOO_LARGE         = "Объем данных слишком велик. Пожалуйста, уточните запрос."
	MSG_LIMIT_EXCEEDED         = "Превышен лимит запросов. Пожалуйста, повторите попытку позже."
	MSG_INVALID_INPUT          = "Недопустимый ввод. Пожалуйста, проверьте формат и повторите попытку."
	MSG_CONFIRMATION_REQUIRED  = "Требуется подтверждение. Используйте кнопки ниже."
)

// Функция для инициализации карты состояний
func initUserStates(app *AppContext) {
	app.UserStates = make(map[int64]string)
	app.UserStatesMutex = &sync.Mutex{}
}

// Установка состояния пользователя
func setUserState(app *AppContext, chatID int64, state string) {
	app.UserStatesMutex.Lock()
	defer app.UserStatesMutex.Unlock()
	app.UserStates[chatID] = state

	// Можно также логировать изменение состояния
	log.Printf("Изменение состояния пользователя %d: %s", chatID, state)
}

// Получение текущего состояния пользователя
func getUserState(app *AppContext, chatID int64) string {
	app.UserStatesMutex.Lock()
	defer app.UserStatesMutex.Unlock()

	state, exists := app.UserStates[chatID]
	if !exists {
		// Если состояние не найдено, проверяем, зарегистрирован ли пользователь
		isRegistered, err := isUserRegistered(app, chatID)
		if err != nil {
			log.Printf("Ошибка проверки регистрации: %v", err)
			return STATE_NEW
		}

		if isRegistered {
			// Устанавливаем состояние для зарегистрированного пользователя
			app.UserStates[chatID] = STATE_WAITING_READINGS
			return STATE_WAITING_READINGS
		} else {
			// Устанавливаем состояние для нового пользователя
			app.UserStates[chatID] = STATE_NEW
			return STATE_NEW
		}
	}

	return state
}

// Удаление состояния пользователя (если нужно)
func clearUserState(app *AppContext, chatID int64) {
	app.UserStatesMutex.Lock()
	defer app.UserStatesMutex.Unlock()
	delete(app.UserStates, chatID)
}

// Инициализация приложения
func initApp() (*AppContext, error) {
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
	for _, dir := range []string{LOG_FOLDER, BACKUP_FOLDER} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("не удалось создать директорию %s: %v", dir, err)
		}
	}

	// Настройка журнала
	logFile, err := os.OpenFile(
		filepath.Join(LOG_FOLDER, fmt.Sprintf("bot_%s.log", time.Now().Format("2006-01-02"))),
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
	ctx, cancel := context.WithTimeout(context.Background(), DATABASE_TIMEOUT)
	defer cancel()

	clientOptions := options.Client().
		ApplyURI(mongoURI).
		SetConnectTimeout(DATABASE_TIMEOUT).
		SetServerSelectionTimeout(DATABASE_TIMEOUT)

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

	// Создание индексов для оптимизации запросов
	createIndexes(ctx, usersCollection, readingsCollection, newsCollection, adminsCollection)

	// Загрузка конфигурации
	var config Config
	err = configCollection.FindOne(ctx, bson.M{}).Decode(&config)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			// Создание конфигурации по умолчанию, если она не существует
			config = Config{
				AdminIDs:             []int64{},
				ReadingReminderDay:   25,
				AllowReadingsFrom:    1,
				AllowReadingsTo:      31,
				MaxColdWaterIncrease: MAX_COLD_WATER_INCREASE,
				MaxHotWaterIncrease:  MAX_HOT_WATER_INCREASE,
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

	// Создание контекста приложения
	app := &AppContext{
		Bot:                bot,
		MongoClient:        client,
		UsersCollection:    usersCollection,
		ReadingsCollection: readingsCollection,
		NewsCollection:     newsCollection,
		AdminsCollection:   adminsCollection,
		ConfigCollection:   configCollection,
		Config:             config,
	}
	// Инициализация карты состояний
	initUserStates(app)

	return app, nil
}

// Создание индексов для оптимизации запросов
func createIndexes(ctx context.Context, usersCollection, readingsCollection, newsCollection, adminsCollection *mongo.Collection) {
	// Индекс для быстрого поиска пользователей по chat_id
	usersCollection.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{"chat_id", 1}},
		Options: options.Index().SetUnique(true),
	})

	// Индекс для быстрого поиска показаний по apartment_number и date
	readingsCollection.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys: bson.D{{"apartment_number", 1}, {"date", -1}},
	})

	// Индекс для новостей
	newsCollection.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys: bson.D{{"date", -1}},
	})

	// Индекс для администраторов
	adminsCollection.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{"chat_id", 1}},
		Options: options.Index().SetUnique(true),
	})
}

// Проверка на администратора
func isAdmin(app *AppContext, chatID int64) bool {
	ctx, cancel := context.WithTimeout(context.Background(), DATABASE_TIMEOUT)
	defer cancel()

	// Проверка в базе данных
	var admin Admin
	err := app.AdminsCollection.FindOne(ctx, bson.M{"chat_id": chatID}).Decode(&admin)
	if err == nil {
		return true
	}

	// Проверка в конфигурации
	for _, id := range app.Config.AdminIDs {
		if id == chatID {
			return true
		}
	}

	return false
}

// Обновление времени последней активности пользователя
func updateUserActivity(app *AppContext, chatID int64) {
	ctx, cancel := context.WithTimeout(context.Background(), DATABASE_TIMEOUT)
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
func isUserRegistered(app *AppContext, chatID int64) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), DATABASE_TIMEOUT)
	defer cancel()

	filter := bson.M{"chat_id": chatID}
	count, err := app.UsersCollection.CountDocuments(ctx, filter)
	if err != nil {
		return false, fmt.Errorf("ошибка проверки регистрации: %v", err)
	}

	return count > 0, nil
}

// Получение данных пользователя
func getUser(app *AppContext, chatID int64) (User, error) {
	ctx, cancel := context.WithTimeout(context.Background(), DATABASE_TIMEOUT)
	defer cancel()

	var user User
	err := app.UsersCollection.FindOne(ctx, bson.M{"chat_id": chatID}).Decode(&user)
	if err != nil {
		return User{}, fmt.Errorf("ошибка получения данных пользователя: %v", err)
	}

	return user, nil
}

// Регистрация нового пользователя
func registerUser(app *AppContext, chatID int64, username string, apartmentNumber int) error {
	ctx, cancel := context.WithTimeout(context.Background(), DATABASE_TIMEOUT)
	defer cancel()

	// Проверка, что квартира не зарегистрирована на другого пользователя
	count, err := app.UsersCollection.CountDocuments(ctx, bson.M{"apartment_number": apartmentNumber})
	if err != nil {
		return fmt.Errorf("ошибка проверки квартиры: %v", err)
	}

	if count > 0 {
		return fmt.Errorf("квартира %d уже зарегистрирована", apartmentNumber)
	}

	// Создание нового пользователя
	user := User{
		ChatID:          chatID,
		Username:        username,
		ApartmentNumber: apartmentNumber,
		RegisteredAt:    time.Now(),
		LastActive:      time.Now(),
	}

	_, err = app.UsersCollection.InsertOne(ctx, user)
	if err != nil {
		return fmt.Errorf("ошибка регистрации: %v", err)
	}

	// Запись в лог
	log.Printf("Зарегистрирован новый пользователь: ChatID=%d, Квартира=%d", chatID, apartmentNumber)

	return nil
}

// Сохранение показаний
func saveReadings(app *AppContext, apartmentNumber int, chatID int64, cold, hot float64) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), DATABASE_TIMEOUT)
	defer cancel()

	// Проверка валидности значений
	if !validateReading(cold) || !validateReading(hot) {
		return "", fmt.Errorf("недопустимые значения показаний")
	}

	// Текущая дата и время приема показаний
	currentTime := time.Now()

	// Создаем новую запись показаний
	reading := Reading{
		ApartmentNumber: apartmentNumber,
		ChatID:          chatID,
		Date:            currentTime,
		Cold:            cold,
		Hot:             hot,
		Verified:        app.Config.AutoConfirmReadings,
	}

	// Вставляем запись в коллекцию readings
	_, err := app.ReadingsCollection.InsertOne(ctx, reading)
	if err != nil {
		return "", fmt.Errorf("ошибка сохранения показаний: %v", err)
	}

	// Форматируем дату для вывода
	formattedTime := currentTime.Format("2006-01-02 15:04")

	// Запись в лог
	log.Printf("Показания сохранены: Квартира=%d, Холодная=%.2f, Горячая=%.2f",
		apartmentNumber, cold, hot)

	return formattedTime, nil
}

// Получение последних показаний для квартиры
func getLastReadings(app *AppContext, apartmentNumber int) (float64, float64, time.Time, error) {
	ctx, cancel := context.WithTimeout(context.Background(), DATABASE_TIMEOUT)
	defer cancel()

	filter := bson.M{"apartment_number": apartmentNumber}
	sort := options.FindOne().SetSort(bson.M{"date": -1}) // Сортировка по убыванию даты

	var lastReading Reading
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
	return value > 0 && value < MAX_WATER_VALUE
}

// Обработка ввода новостей администратором
func handleNewsInput(app *AppContext, update tgbotapi.Update) {
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
	setUserState(app, chatID, STATE_WAITING_READINGS)

	// Отправляем подтверждение
	msg := tgbotapi.NewMessage(chatID, "Новость успешно добавлена. Используйте /sendnews для отправки.")
	app.Bot.Send(msg)
}

// Добавление новости
func addNews(app *AppContext, title, content, createdBy string) error {
	ctx, cancel := context.WithTimeout(context.Background(), DATABASE_TIMEOUT)
	defer cancel()

	// Проверка длины новости
	if len(title) > 100 || len(content) > 1000 {
		return fmt.Errorf("слишком длинное содержание новости")
	}

	news := News{
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

// Отправка всех новостей пользователям
func sendNewsToUsers(app *AppContext) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), DATABASE_TIMEOUT)
	defer cancel()

	// Находим все непосланные новости
	filter := bson.M{"sent": false}
	newsCursor, err := app.NewsCollection.Find(ctx, filter, options.Find().SetSort(bson.M{"date": -1}))
	if err != nil {
		return 0, fmt.Errorf("ошибка получения новостей: %v", err)
	}
	defer newsCursor.Close(ctx)

	// Преобразуем новости в сообщения
	var newsMessages []News
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

	var users []User
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

// Отправка напоминаний
func sendReminders(app *AppContext) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), DATABASE_TIMEOUT)
	defer cancel()

	// Текущая дата
	now := time.Now()
	currentMonth := now.Month()
	currentYear := now.Year()

	// Начало текущего месяца
	startOfMonth := time.Date(currentYear, currentMonth, 1, 0, 0, 0, 0, time.UTC)

	// Находим всех зарегистрированных пользователей
	usersCursor, err := app.UsersCollection.Find(ctx, bson.M{})
	if err != nil {
		return 0, fmt.Errorf("ошибка получения списка пользователей: %v", err)
	}
	defer usersCursor.Close(ctx)

	var users []User
	if err = usersCursor.All(ctx, &users); err != nil {
		return 0, fmt.Errorf("ошибка чтения пользователей: %v", err)
	}

	remindersSent := 0
	// Для каждого пользователя проверяем последние показания
	for _, user := range users {
		// Получаем показания за текущий месяц
		filter := bson.M{
			"apartment_number": user.ApartmentNumber,
			"date":             bson.M{"$gte": startOfMonth},
		}

		count, err := app.ReadingsCollection.CountDocuments(ctx, filter)
		if err != nil {
			log.Printf("Ошибка проверки показаний для квартиры %d: %v", user.ApartmentNumber, err)
			continue
		}

		// Если показания за этот месяц не переданы, отправляем напоминание
		if count == 0 {
			msg := tgbotapi.NewMessage(user.ChatID, MSG_REMINDER)
			_, err := app.Bot.Send(msg)
			if err != nil {
				log.Printf("Ошибка отправки напоминания пользователю %d: %v", user.ChatID, err)
				continue
			}
			remindersSent++

			// Добавляем небольшую задержку
			time.Sleep(100 * time.Millisecond)
		}
	}

	return remindersSent, nil
}

// Получение истории показаний для квартиры
func getHistory(app *AppContext, apartmentNumber int, limit int) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), DATABASE_TIMEOUT)
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

	var readings []Reading
	if err = cursor.All(ctx, &readings); err != nil {
		return "", fmt.Errorf("ошибка чтения показаний: %v", err)
	}

	if len(readings) == 0 {
		return MSG_NO_READINGS, nil
	}

	// Формирование ответа
	response := MSG_HISTORY_TITLE
	for i, reading := range readings {
		response += fmt.Sprintf(
			MSG_HISTORY_ENTRY,
			i+1,
			reading.Cold,
			reading.Hot,
			reading.Date.Format("2006-01-02"),
		)
	}

	return response, nil
}

// Получение статистики потребления
func getStatistics(app *AppContext, apartmentNumber int) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), DATABASE_TIMEOUT)
	defer cancel()

	// Находим все показания для квартиры, отсортированные по дате
	filter := bson.M{"apartment_number": apartmentNumber}
	options := options.Find().SetSort(bson.M{"date": 1})

	cursor, err := app.ReadingsCollection.Find(ctx, filter, options)
	if err != nil {
		return "", fmt.Errorf("ошибка получения показаний: %v", err)
	}
	defer cursor.Close(ctx)

	var readings []Reading
	if err = cursor.All(ctx, &readings); err != nil {
		return "", fmt.Errorf("ошибка чтения показаний: %v", err)
	}

	if len(readings) < 2 {
		return "Недостаточно данных для расчета статистики. Требуется минимум 2 показания.", nil
	}

	// Переменные для статистики по абсолютным значениям
	var totalCold, totalHot float64
	var maxCold, maxHot float64 = 0, 0
	var minCold, minHot float64
	var maxColdDate, maxHotDate, minColdDate, minHotDate time.Time

	// Инициализируем минимальные и максимальные значения первым показанием
	if len(readings) > 0 {
		minCold = readings[0].Cold
		minHot = readings[0].Hot
		maxCold = readings[0].Cold
		maxHot = readings[0].Hot
		minColdDate = readings[0].Date
		minHotDate = readings[0].Date
		maxColdDate = readings[0].Date
		maxHotDate = readings[0].Date
	} else {
		minCold, minHot = 9999.99, 9999.99
	}

	// Расчет статистики по абсолютным значениям
	for _, reading := range readings {
		totalCold += reading.Cold
		totalHot += reading.Hot

		if reading.Cold > maxCold {
			maxCold = reading.Cold
			maxColdDate = reading.Date
		}
		if reading.Cold < minCold {
			minCold = reading.Cold
			minColdDate = reading.Date
		}

		if reading.Hot > maxHot {
			maxHot = reading.Hot
			maxHotDate = reading.Date
		}
		if reading.Hot < minHot {
			minHot = reading.Hot
			minHotDate = reading.Date
		}
	}

	avgCold := totalCold / float64(len(readings))
	avgHot := totalHot / float64(len(readings))

	// Форматирование ответа
	return fmt.Sprintf(
		MSG_STATISTICS,
		apartmentNumber,
		avgCold,
		maxCold, maxColdDate.Format("01/2006"),
		minCold, minColdDate.Format("01/2006"),
		avgHot,
		maxHot, maxHotDate.Format("01/2006"),
		minHot, minHotDate.Format("01/2006"),
	), nil
}

// Экспорт показаний в CSV
func exportReadingsToCSV(app *AppContext, apartmentNumber int) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), DATABASE_TIMEOUT)
	defer cancel()

	filter := bson.M{"apartment_number": apartmentNumber}
	options := options.Find().SetSort(bson.M{"date": 1})

	cursor, err := app.ReadingsCollection.Find(ctx, filter, options)
	if err != nil {
		return "", fmt.Errorf("ошибка получения показаний: %v", err)
	}
	defer cursor.Close(ctx)

	var readings []Reading
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
func createBackup(app *AppContext) error {
	ctx, cancel := context.WithTimeout(context.Background(), DATABASE_TIMEOUT*2)
	defer cancel()

	// Создаем директорию для резервной копии
	backupTime := time.Now().Format("20060102_150405")
	backupDir := filepath.Join(BACKUP_FOLDER, backupTime)
	if err := os.MkdirAll(backupDir, 0o755); err != nil {
		return fmt.Errorf("ошибка создания директории бэкапа: %v", err)
	}

	// Получаем списки коллекций для резервного копирования
	collections := []struct {
		Collection *mongo.Collection
		Name       string
	}{
		{app.UsersCollection, "users"},
		{app.ReadingsCollection, "readings"},
		{app.NewsCollection, "news"},
		{app.AdminsCollection, "admins"},
		{app.ConfigCollection, "config"},
	}

	// Экспортируем каждую коллекцию в JSON файл
	for _, coll := range collections {
		cursor, err := coll.Collection.Find(ctx, bson.M{})
		if err != nil {
			return fmt.Errorf("ошибка чтения коллекции %s: %v", coll.Name, err)
		}

		var documents []bson.M
		if err = cursor.All(ctx, &documents); err != nil {
			cursor.Close(ctx)
			return fmt.Errorf("ошибка декодирования документов из %s: %v", coll.Name, err)
		}
		cursor.Close(ctx)

		// Создаем файл для коллекции
		filePath := filepath.Join(backupDir, coll.Name+".json")
		file, err := os.Create(filePath)
		if err != nil {
			return fmt.Errorf("ошибка создания файла %s: %v", filePath, err)
		}

		// Сериализуем документы в JSON
		for _, doc := range documents {
			jsonData, err := bson.MarshalExtJSON(doc, true, false)
			if err != nil {
				file.Close()
				return fmt.Errorf("ошибка маршалинга JSON для %s: %v", coll.Name, err)
			}
			file.Write(jsonData)
			file.Write([]byte("\n"))
		}
		file.Close()
	}

	log.Printf("Резервная копия создана: %s", backupDir)
	return nil
}

// Периодические задачи
func startScheduledTasks(app *AppContext) {
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

	entries, err := os.ReadDir(BACKUP_FOLDER)
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
			backupPath := filepath.Join(BACKUP_FOLDER, entry.Name())
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
func handleStart(app *AppContext, update tgbotapi.Update) {
	chatID := update.Message.Chat.ID
	//	username := update.Message.From.UserName

	// Проверка режима обслуживания
	if app.Config.MaintenanceMode && !isAdmin(app, chatID) {
		msg := tgbotapi.NewMessage(chatID, app.Config.MaintenanceMessage)
		app.Bot.Send(msg)
		return
	}

	isRegistered, err := isUserRegistered(app, chatID)
	if err != nil {
		log.Printf("Ошибка проверки регистрации: %v", err)
		msg := tgbotapi.NewMessage(chatID, MSG_ERROR_OCCURRED)
		app.Bot.Send(msg)
		return
	}

	if isRegistered {
		// Обновляем активность пользователя
		setUserState(app, chatID, STATE_WAITING_READINGS)
		updateUserActivity(app, chatID)

		// Пользователь уже зарегистрирован
		msg := tgbotapi.NewMessage(chatID, MSG_START_REGISTERED)
		app.Bot.Send(msg)
	} else {
		// Пользователь не зарегистрирован
		setUserState(app, chatID, STATE_WAITING_APARTMENT)
		msg := tgbotapi.NewMessage(
			chatID,
			fmt.Sprintf(MSG_START_NOT_REGISTERED, MIN_APARTMENT_NUMBER, MAX_APARTMENT_NUMBER),
		)
		app.Bot.Send(msg)
	}
}

// Обработка команды /history
func handleHistory(app *AppContext, update tgbotapi.Update) {
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
		msg := tgbotapi.NewMessage(chatID, MSG_ERROR_OCCURRED)
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
		msg := tgbotapi.NewMessage(chatID, MSG_ERROR_OCCURRED)
		app.Bot.Send(msg)
		return
	}

	// Получаем историю показаний
	history, err := getHistory(app, user.ApartmentNumber, 12)
	if err != nil {
		log.Printf("Ошибка получения истории показаний: %v", err)
		msg := tgbotapi.NewMessage(chatID, MSG_ERROR_OCCURRED)
		app.Bot.Send(msg)
		return
	}

	// Отправляем историю пользователю
	msg := tgbotapi.NewMessage(chatID, history)
	app.Bot.Send(msg)
}

// Обработка команды /stats
func handleStats(app *AppContext, update tgbotapi.Update) {
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
		msg := tgbotapi.NewMessage(chatID, MSG_ERROR_OCCURRED)
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
		msg := tgbotapi.NewMessage(chatID, MSG_ERROR_OCCURRED)
		app.Bot.Send(msg)
		return
	}

	// Получаем статистику
	stats, err := getStatistics(app, user.ApartmentNumber)
	if err != nil {
		log.Printf("Ошибка получения статистики: %v", err)
		msg := tgbotapi.NewMessage(chatID, MSG_ERROR_OCCURRED)
		app.Bot.Send(msg)
		return
	}

	// Отправляем статистику пользователю
	msg := tgbotapi.NewMessage(chatID, stats)
	app.Bot.Send(msg)
}

// Обработка регистрации нового пользователя
func handleRegistration(app *AppContext, update tgbotapi.Update) {
	chatID := update.Message.Chat.ID
	messageText := update.Message.Text

	// Проверяем формат номера квартиры
	apartmentNumberStr := strings.TrimSpace(messageText)
	apartmentNumber, err := strconv.Atoi(apartmentNumberStr)
	if err != nil || apartmentNumber < MIN_APARTMENT_NUMBER || apartmentNumber > MAX_APARTMENT_NUMBER {
		msg := tgbotapi.NewMessage(chatID, fmt.Sprintf(MSG_INVALID_APARTMENT, MIN_APARTMENT_NUMBER, MAX_APARTMENT_NUMBER))
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
	msg := tgbotapi.NewMessage(chatID, fmt.Sprintf(MSG_REGISTRATION_SUCCESS, apartmentNumber))
	app.Bot.Send(msg)

	// Просим пользователя ввести показания
	msg = tgbotapi.NewMessage(chatID, MSG_START_REGISTERED)
	app.Bot.Send(msg)
}

// Обработка команды /export
func handleExport(app *AppContext, update tgbotapi.Update) {
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
		msg := tgbotapi.NewMessage(chatID, MSG_ERROR_OCCURRED)
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
		msg := tgbotapi.NewMessage(chatID, MSG_ERROR_OCCURRED)
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
func handleAddNews(app *AppContext, update tgbotapi.Update) {
	chatID := update.Message.Chat.ID

	// Проверяем, является ли пользователь админом
	if !isAdmin(app, chatID) {
		msg := tgbotapi.NewMessage(chatID, MSG_NO_PERMISSION)
		app.Bot.Send(msg)
		return
	}
	// Устанавливаем состояние ожидания ввода новости
	setUserState(app, chatID, STATE_ADMIN_WAITING_NEWS)
	// Проверяем формат команды
	args := strings.TrimSpace(update.Message.CommandArguments())
	parts := strings.SplitN(args, " ", 2)
	if len(parts) != 2 {
		msg := tgbotapi.NewMessage(chatID, MSG_ADDNEWS_USAGE)
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
	msg := tgbotapi.NewMessage(chatID, MSG_NEWS_ADDED)
	app.Bot.Send(msg)
}

// Обработка команды /sendnews (только для админов)
func handleSendNews(app *AppContext, update tgbotapi.Update) {
	chatID := update.Message.Chat.ID

	// Проверяем, является ли пользователь админом
	if !isAdmin(app, chatID) {
		msg := tgbotapi.NewMessage(chatID, MSG_NO_PERMISSION)
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
func handleRemind(app *AppContext, update tgbotapi.Update) {
	chatID := update.Message.Chat.ID

	// Проверяем, является ли пользователь админом
	if !isAdmin(app, chatID) {
		msg := tgbotapi.NewMessage(chatID, MSG_NO_PERMISSION)
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
func handleBackup(app *AppContext, update tgbotapi.Update) {
	chatID := update.Message.Chat.ID

	// Проверяем, является ли пользователь админом
	if !isAdmin(app, chatID) {
		msg := tgbotapi.NewMessage(chatID, MSG_NO_PERMISSION)
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
	edit := tgbotapi.NewEditMessageText(chatID, sentMsg.MessageID, MSG_BACKUP_COMPLETED)
	app.Bot.Send(edit)
}

// Обработка команды /maintenance (только для админов)
func handleMaintenance(app *AppContext, update tgbotapi.Update) {
	chatID := update.Message.Chat.ID

	// Проверяем, является ли пользователь админом
	if !isAdmin(app, chatID) {
		msg := tgbotapi.NewMessage(chatID, MSG_NO_PERMISSION)
		app.Bot.Send(msg)
		return
	}

	// Получаем аргументы команды
	args := strings.TrimSpace(update.Message.CommandArguments())

	ctx, cancel := context.WithTimeout(context.Background(), DATABASE_TIMEOUT)
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
func handleReadings(app *AppContext, update tgbotapi.Update) {
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
		msg := tgbotapi.NewMessage(chatID, MSG_INVALID_FORMAT)
		app.Bot.Send(msg)
		return
	}

	// Проверяем корректность формата данных
	cold, err := strconv.ParseFloat(parts[0], 64)
	if err != nil || cold <= 0 {
		msg := tgbotapi.NewMessage(chatID, MSG_VALUE_MUST_BE_POSITIVE)
		app.Bot.Send(msg)
		return
	}

	hot, err := strconv.ParseFloat(parts[1], 64)
	if err != nil || hot <= 0 {
		msg := tgbotapi.NewMessage(chatID, MSG_VALUE_MUST_BE_POSITIVE)
		app.Bot.Send(msg)
		return
	}

	// Проверяем, что значения не превышают максимально допустимые
	if cold > MAX_WATER_VALUE || hot > MAX_WATER_VALUE {
		msg := tgbotapi.NewMessage(chatID, fmt.Sprintf("Значение не может быть больше %.0f.", MAX_WATER_VALUE))
		app.Bot.Send(msg)
		return
	}

	// Получаем данные пользователя
	user, err := getUser(app, chatID)
	if err != nil {
		log.Printf("Ошибка получения данных пользователя: %v", err)
		msg := tgbotapi.NewMessage(chatID, MSG_ERROR_OCCURRED)
		app.Bot.Send(msg)
		return
	}

	// Получаем последние показания для квартиры
	//	lastCold, lastHot, lastDate, err := getLastReadings(app, user.ApartmentNumber)
	lastCold, lastHot, _, err := getLastReadings(app, user.ApartmentNumber)
	if err != nil {
		log.Printf("Ошибка получения последних показаний: %v", err)
		msg := tgbotapi.NewMessage(chatID, MSG_ERROR_OCCURRED)
		app.Bot.Send(msg)
		return
	}

	// Проверяем, что новые показания больше или равны предыдущим
	if lastCold > 0 && cold < lastCold {
		msg := tgbotapi.NewMessage(chatID, fmt.Sprintf(MSG_COLD_DECREASED, cold, lastCold))
		app.Bot.Send(msg)
		return
	}

	if lastHot > 0 && hot < lastHot {
		msg := tgbotapi.NewMessage(chatID, fmt.Sprintf(MSG_HOT_DECREASED, hot, lastHot))
		app.Bot.Send(msg)
		return
	}

	// Проверяем разницу между новыми и предыдущими показаниями
	if lastCold > 0 && (cold-lastCold) > app.Config.MaxColdWaterIncrease {
		// Показания вызывают подозрение, запрашиваем подтверждение
		msg := tgbotapi.NewMessage(
			chatID,
			fmt.Sprintf(MSG_LARGE_DIFFERENCE_COLD, cold-lastCold)+"\n\nХолодная вода: "+fmt.Sprintf("%.2f → %.2f", lastCold, cold),
		)
		msg.ReplyMarkup = createConfirmationKeyboard()
		app.Bot.Send(msg)
		return
	}

	if lastHot > 0 && (hot-lastHot) > app.Config.MaxHotWaterIncrease {
		// Показания вызывают подозрение, запрашиваем подтверждение
		msg := tgbotapi.NewMessage(
			chatID,
			fmt.Sprintf(MSG_LARGE_DIFFERENCE_HOT, hot-lastHot)+"\n\nГорячая вода: "+fmt.Sprintf("%.2f → %.2f", lastHot, hot),
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
	msg := tgbotapi.NewMessage(chatID, fmt.Sprintf(MSG_READINGS_SUCCESS, cold, hot, receivedTime))
	app.Bot.Send(msg)
}

// Обработка ввода показаний
func handleReadingsInput(app *AppContext, update tgbotapi.Update) {
	chatID := update.Message.Chat.ID
	messageText := update.Message.Text

	// Получаем данные пользователя
	user, err := getUser(app, chatID)
	if err != nil {
		log.Printf("Ошибка получения данных пользователя: %v", err)
		msg := tgbotapi.NewMessage(chatID, MSG_ERROR_OCCURRED)
		app.Bot.Send(msg)
		return
	}

	// Обработка показаний счетчиков
	parts := strings.Fields(messageText)
	if len(parts) != 2 {
		msg := tgbotapi.NewMessage(chatID, MSG_INVALID_FORMAT)
		app.Bot.Send(msg)
		return
	}

	// Проверяем корректность формата данных
	cold, err := strconv.ParseFloat(parts[0], 64)
	if err != nil || cold <= 0 {
		msg := tgbotapi.NewMessage(chatID, MSG_VALUE_MUST_BE_POSITIVE)
		app.Bot.Send(msg)
		return
	}

	hot, err := strconv.ParseFloat(parts[1], 64)
	if err != nil || hot <= 0 {
		msg := tgbotapi.NewMessage(chatID, MSG_VALUE_MUST_BE_POSITIVE)
		app.Bot.Send(msg)
		return
	}

	// Проверяем, что значения не превышают максимально допустимые
	if cold > MAX_WATER_VALUE || hot > MAX_WATER_VALUE {
		msg := tgbotapi.NewMessage(chatID, fmt.Sprintf("Значение не может быть больше %.0f.", MAX_WATER_VALUE))
		app.Bot.Send(msg)
		return
	}

	// Получаем последние показания для квартиры
	lastCold, lastHot, _, err := getLastReadings(app, user.ApartmentNumber)
	if err != nil {
		log.Printf("Ошибка получения последних показаний: %v", err)
		msg := tgbotapi.NewMessage(chatID, MSG_ERROR_OCCURRED)
		app.Bot.Send(msg)
		return
	}

	// Проверяем, что новые показания больше или равны предыдущим
	if lastCold > 0 && cold < lastCold {
		msg := tgbotapi.NewMessage(chatID, fmt.Sprintf(MSG_COLD_DECREASED, cold, lastCold))
		app.Bot.Send(msg)
		return
	}

	if lastHot > 0 && hot < lastHot {
		msg := tgbotapi.NewMessage(chatID, fmt.Sprintf(MSG_HOT_DECREASED, hot, lastHot))
		app.Bot.Send(msg)
		return
	}

	// Проверяем разницу между новыми и предыдущими показаниями
	if lastCold > 0 && (cold-lastCold) > app.Config.MaxColdWaterIncrease {
		// Показания вызывают подозрение, запрашиваем подтверждение
		setUserState(app, chatID, STATE_CONFIRMING_READINGS)
		msg := tgbotapi.NewMessage(
			chatID,
			fmt.Sprintf(MSG_LARGE_DIFFERENCE_COLD, cold-lastCold)+"\n\nХолодная вода: "+fmt.Sprintf("%.2f → %.2f", lastCold, cold),
		)
		msg.ReplyMarkup = createConfirmationKeyboard()
		app.Bot.Send(msg)
		return
	}

	if lastHot > 0 && (hot-lastHot) > app.Config.MaxHotWaterIncrease {
		// Показания вызывают подозрение, запрашиваем подтверждение
		setUserState(app, chatID, STATE_CONFIRMING_READINGS)
		msg := tgbotapi.NewMessage(
			chatID,
			fmt.Sprintf(MSG_LARGE_DIFFERENCE_HOT, hot-lastHot)+"\n\nГорячая вода: "+fmt.Sprintf("%.2f → %.2f", lastHot, hot),
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
	msg := tgbotapi.NewMessage(chatID, fmt.Sprintf(MSG_READINGS_SUCCESS, cold, hot, receivedTime))
	app.Bot.Send(msg)
}

// Обработка нажатий на инлайн-кнопки
func handleCallbackQuery(app *AppContext, update tgbotapi.Update) {
	callback := update.CallbackQuery
	chatID := callback.Message.Chat.ID
	data := callback.Data

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
		setUserState(app, chatID, STATE_WAITING_READINGS)
		return
	}
	if data == "confirm" {
		// Проверяем состояние пользователя
		state := getUserState(app, chatID)

		if state == STATE_CONFIRMING_READINGS {
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
				setUserState(app, chatID, STATE_WAITING_READINGS)
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
				fmt.Sprintf(MSG_READINGS_SUCCESS, cold, hot, receivedTime),
			)
			app.Bot.Send(confirmMsg)
			setUserState(app, chatID, STATE_WAITING_READINGS)
		}
	}
} // Обработка регистрации квартиры
func handleApartmentRegistration(app *AppContext, update tgbotapi.Update) {
	chatID := update.Message.Chat.ID
	messageText := update.Message.Text

	// Проверяем формат номера квартиры
	apartmentNumberStr := strings.TrimSpace(messageText)
	apartmentNumber, err := strconv.Atoi(apartmentNumberStr)
	if err != nil || apartmentNumber < MIN_APARTMENT_NUMBER || apartmentNumber > MAX_APARTMENT_NUMBER {
		msg := tgbotapi.NewMessage(chatID, fmt.Sprintf(MSG_INVALID_APARTMENT, MIN_APARTMENT_NUMBER, MAX_APARTMENT_NUMBER))
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
	msg := tgbotapi.NewMessage(chatID, fmt.Sprintf(MSG_REGISTRATION_SUCCESS, apartmentNumber))
	app.Bot.Send(msg)

	// Меняем состояние на ожидание показаний
	setUserState(app, chatID, STATE_WAITING_READINGS)

	// Просим пользователя ввести показания
	msg = tgbotapi.NewMessage(chatID, MSG_START_REGISTERED)
	app.Bot.Send(msg)
}

// Обработка текстовых сообщений
// Обработка текстовых сообщений
func handleMessage(app *AppContext, update tgbotapi.Update) {
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
	case STATE_NEW:
		// Предлагаем новому пользователю начать процесс регистрации
		setUserState(app, chatID, STATE_WAITING_APARTMENT)
		msg := tgbotapi.NewMessage(
			chatID,
			fmt.Sprintf(MSG_START_NOT_REGISTERED, MIN_APARTMENT_NUMBER, MAX_APARTMENT_NUMBER),
		)
		app.Bot.Send(msg)

	case STATE_WAITING_APARTMENT:
		// Обрабатываем ввод номера квартиры
		handleApartmentRegistration(app, update)

	case STATE_WAITING_READINGS:
		// Обрабатываем ввод показаний
		handleReadingsInput(app, update)

	case STATE_CONFIRMING_READINGS:
		// Обрабатываем подтверждение показаний (для случаев с большой разницей)
		// Этот случай обычно обрабатывается через CallbackQuery, но можно добавить текстовое подтверждение
		msg := tgbotapi.NewMessage(chatID, "Пожалуйста, используйте кнопки подтверждения ниже сообщения.")
		app.Bot.Send(msg)

	case STATE_ADMIN_WAITING_NEWS:
		// Только для админов - обработка ввода текста новости
		if isAdmin(app, chatID) {
			handleNewsInput(app, update)
		} else {
			setUserState(app, chatID, STATE_WAITING_READINGS)
			msg := tgbotapi.NewMessage(chatID, MSG_NO_PERMISSION)
			app.Bot.Send(msg)
		}

	default:
		// Неизвестное состояние - сбрасываем на ожидание показаний
		log.Printf("Неизвестное состояние пользователя %d: %s", chatID, state)
		setUserState(app, chatID, STATE_WAITING_READINGS)
		msg := tgbotapi.NewMessage(chatID, MSG_START_REGISTERED)
		app.Bot.Send(msg)
	}
}

// Общая функция обработки всех команд
func handleCommand(app *AppContext, update tgbotapi.Update) {
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
	default:
		// Неизвестная команда
		msg := tgbotapi.NewMessage(update.Message.Chat.ID, "Неизвестная команда. Введите /start для начала работы.")
		app.Bot.Send(msg)
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

func main() {
	// Инициализация приложения
	app, err := initApp()
	if err != nil {
		log.Fatalf("Ошибка инициализации: %v", err)
	}
	defer app.MongoClient.Disconnect(context.Background())

	log.Printf("Бот запущен: @%s", app.Bot.Self.UserName)

	// Запускаем периодические задачи
	startScheduledTasks(app)

	// Создаем лимитер для защиты от спама
	canProcess := rateLimiter()

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
			msg := tgbotapi.NewMessage(chatID, MSG_LIMIT_EXCEEDED)
			app.Bot.Send(msg)
			continue
		}

		// Проверка длины сообщения
		if len(update.Message.Text) > MAX_MESSAGE_LENGTH {
			msg := tgbotapi.NewMessage(chatID, MSG_DATA_TOO_LARGE)
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
					msg := tgbotapi.NewMessage(u.Message.Chat.ID, MSG_ERROR_OCCURRED)
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
