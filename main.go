package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/joho/godotenv" // Библиотека для работы с .env
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// Константы для минимального и максимального номера квартиры и ID администратора
const (
	MIN_APARTMENT_NUMBER = 1         // Минимальный номер квартиры
	MAX_APARTMENT_NUMBER = 500       // Максимальный номер квартиры
	ADMIN_ID             = 123456789 // ID администратора (замените на реальный ID)
)

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
)

// Загрузка переменных окружения
func loadEnv() {
	err := godotenv.Load(".env") // Загрузка переменных из файла .env
	if err != nil {
		log.Fatalf("Ошибка загрузки .env файла: %v", err)
	}
}

// Проверка существования пользователя по chat_id
func isUserRegistered(usersCollection *mongo.Collection, chatID int) (bool, error) {
	filter := bson.M{"chat_id": chatID}
	result := usersCollection.FindOne(context.TODO(), filter)

	// Если документ не найден, возвращаем false без ошибки
	if result.Err() == mongo.ErrNoDocuments {
		return false, nil
	}

	// Возвращаем true, если документ найден, или передаем другую ошибку
	return result.Err() == nil, result.Err()
}

// Регистрация нового пользователя
func registerUser(usersCollection *mongo.Collection, chatID int, apartmentNumber int) error {
	_, err := usersCollection.InsertOne(context.TODO(), bson.M{
		"chat_id":          chatID,
		"apartment_number": apartmentNumber,
	})
	return err
}

// Сохранение показаний
func saveReadings(readingsCollection *mongo.Collection, apartmentNumber int, cold, hot float64) (string, error) {
	// Текущая дата и время приема показаний
	currentTime := time.Now()

	// Создаем новую запись показаний
	reading := bson.M{
		"apartment_number": apartmentNumber,
		"date":             currentTime,
		"cold":             cold,
		"hot":              hot,
	}

	// Вставляем запись в коллекцию readings
	_, err := readingsCollection.InsertOne(context.TODO(), reading)
	if err != nil {
		return "", err
	}

	// Форматируем дату для вывода
	formattedTime := currentTime.Format("2006-01-02 15:04")
	return formattedTime, nil
}

// Получение последних показаний для квартиры
func getLastReadings(readingsCollection *mongo.Collection, apartmentNumber int) (float64, float64, error) {
	filter := bson.M{"apartment_number": apartmentNumber}
	sort := options.FindOne().SetSort(bson.M{"date": -1}) // Сортировка по убыванию даты

	var lastReading struct {
		Cold float64 `bson:"cold"`
		Hot  float64 `bson:"hot"`
	}

	err := readingsCollection.FindOne(context.TODO(), filter, sort).Decode(&lastReading)
	if err != nil && err != mongo.ErrNoDocuments {
		return 0, 0, err
	}

	// Если показания отсутствуют, возвращаем нулевые значения
	if err == mongo.ErrNoDocuments {
		return 0, 0, nil
	}

	return lastReading.Cold, lastReading.Hot, nil
}

// Добавление новости
func addNews(newsCollection *mongo.Collection, title, content string) error {
	_, err := newsCollection.InsertOne(context.TODO(), bson.M{
		"title":   title,
		"content": content,
		"date":    time.Now(),
	})
	return err
}

// Отправка всех новостей пользователям
func sendNewsToUsers(bot *tgbotapi.BotAPI, usersCollection, newsCollection *mongo.Collection) {
	// Находим всех зарегистрированных пользователей
	cursor, err := usersCollection.Find(context.TODO(), bson.M{})
	if err != nil {
		log.Printf("Ошибка получения списка пользователей: %v", err)
		return
	}
	defer cursor.Close(context.TODO())

	// Находим все новости
	newsCursor, err := newsCollection.Find(context.TODO(), bson.M{}, options.Find().SetSort(bson.M{"date": -1}))
	if err != nil {
		log.Printf("Ошибка получения новостей: %v", err)
		return
	}
	defer newsCursor.Close(context.TODO())

	// Преобразуем новости в сообщения
	var newsMessages []string
	for newsCursor.Next(context.TODO()) {
		var news struct {
			Title   string    `bson:"title"`
			Content string    `bson:"content"`
			Date    time.Time `bson:"date"`
		}
		err := newsCursor.Decode(&news)
		if err != nil {
			log.Printf("Ошибка декодирования новости: %v", err)
			continue
		}
		newsMessages = append(newsMessages, fmt.Sprintf(
			"❗️ Новость (%s):\n%s\nДата: %s",
			news.Title,
			news.Content,
			news.Date.Format("2006-01-02"),
		))
	}

	// Отправляем новости каждому пользователю
	for cursor.Next(context.TODO()) {
		var user struct {
			ChatID          int `bson:"chat_id"`
			ApartmentNumber int `bson:"apartment_number"`
		}
		err := cursor.Decode(&user)
		if err != nil {
			log.Printf("Ошибка декодирования пользователя: %v", err)
			continue
		}

		for _, message := range newsMessages {
			msg := tgbotapi.NewMessage(int64(user.ChatID), message)
			_, err := bot.Send(msg)
			if err != nil {
				log.Printf("Ошибка отправки новости пользователю %d: %v", user.ChatID, err)
				continue
			}
		}
	}
}

// Проверка необходимости напоминания о показаниях
func checkAndSendReminders(bot *tgbotapi.BotAPI, usersCollection, readingsCollection *mongo.Collection) {
	// Текущая дата
	now := time.Now()
	deadline := time.Date(now.Year(), now.Month(), 10, 0, 0, 0, 0, time.UTC) // Срок до 10-го числа месяца

	// Находим всех зарегистрированных пользователей
	cursor, err := usersCollection.Find(context.TODO(), bson.M{})
	if err != nil {
		log.Printf("Ошибка получения списка пользователей: %v", err)
		return
	}
	defer cursor.Close(context.TODO())

	// Для каждого пользователя проверяем последние показания
	for cursor.Next(context.TODO()) {
		var user struct {
			ChatID          int `bson:"chat_id"`
			ApartmentNumber int `bson:"apartment_number"`
		}
		err := cursor.Decode(&user)
		if err != nil {
			log.Printf("Ошибка декодирования пользователя: %v", err)
			continue
		}

		// Находим последние показания для квартиры
		lastCold, lastHot, err := getLastReadings(readingsCollection, user.ApartmentNumber)
		if err != nil && err != mongo.ErrNoDocuments {
			log.Printf("Ошибка получения последних показаний для квартиры %d: %v", user.ApartmentNumber, err)
			continue
		}

		// Получаем дату последнего показания
		var lastReadingDate time.Time
		if lastCold > 0 && lastHot > 0 {
			var lastReading struct {
				Date time.Time `bson:"date"`
			}
			err := readingsCollection.FindOne(context.TODO(), bson.M{"apartment_number": user.ApartmentNumber}, options.FindOne().SetSort(bson.M{"date": -1})).Decode(&lastReading)
			if err != nil && err != mongo.ErrNoDocuments {
				log.Printf("Ошибка получения даты последнего показания для квартиры %d: %v", user.ApartmentNumber, err)
				continue
			}
			lastReadingDate = lastReading.Date
		}

		// Если показания отсутствуют или они старые, отправляем напоминание
		if lastCold == 0 && lastHot == 0 || (lastCold > 0 && lastHot > 0 && !lastReadingDate.IsZero() && lastReadingDate.Before(deadline)) {
			msg := tgbotapi.NewMessage(int64(user.ChatID), MSG_REMINDER)
			_, err := bot.Send(msg)
			if err != nil {
				log.Printf("Ошибка отправки напоминания пользователю %d: %v", user.ChatID, err)
				continue
			}
		}
	}
}

// Получение истории показаний для квартиры
func getHistory(readingsCollection *mongo.Collection, apartmentNumber int) (string, error) {
	filter := bson.M{"apartment_number": apartmentNumber}
	sort := options.Find().SetSort(bson.M{"date": -1}) // Сортировка по убыванию даты
	limit := options.Find().SetLimit(12)               // Ограничение до 12 записей

	cursor, err := readingsCollection.Find(context.TODO(), filter, sort, limit)
	if err != nil {
		return "", err
	}
	defer cursor.Close(context.TODO())

	// Формирование ответа
	response := MSG_HISTORY_TITLE
	for i := 0; cursor.Next(context.TODO()); i++ {
		var reading struct {
			Cold float64   `bson:"cold"`
			Hot  float64   `bson:"hot"`
			Date time.Time `bson:"date"`
		}

		err := cursor.Decode(&reading)
		if err != nil {
			return "", err
		}

		response += fmt.Sprintf(
			MSG_HISTORY_ENTRY,
			i+1,
			reading.Cold,
			reading.Hot,
			reading.Date.Format("2006-01-02"),
		)
	}

	if response == MSG_HISTORY_TITLE {
		return MSG_NO_READINGS, nil
	}

	return response, nil
}

func main() {
	loadEnv()

	// Получение токена Telegram и строки подключения MongoDB
	token := os.Getenv("TELEGRAM_TOKEN")
	mongoURI := os.Getenv("MONGO_URI")

	if token == "" || mongoURI == "" {
		log.Fatal("Не указан TELEGRAM_TOKEN или MONGO_URI")
	}

	// Инициализация Telegram бота
	bot, err := tgbotapi.NewBotAPI(token)
	if err != nil {
		log.Fatalf("Ошибка при создании бота: %v", err)
	}

	bot.Debug = true
	log.Printf("Авторизован на аккаунт: %s", bot.Self.UserName)

	// Подключение к MongoDB
	client, err := mongo.Connect(context.TODO(), options.Client().ApplyURI(mongoURI))
	if err != nil {
		log.Fatalf("Ошибка подключения к MongoDB: %v", err)
	}
	defer client.Disconnect(context.TODO())

	usersCollection := client.Database("water_meters").Collection("users")
	readingsCollection := client.Database("water_meters").Collection("readings")
	newsCollection := client.Database("water_meters").Collection("news")

	// Обработка команд
	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60

	updates := bot.GetUpdatesChan(u)

	for update := range updates {
		if update.Message == nil {
			continue
		}

		chatID := update.Message.From.ID
		log.Printf("[%s] %s", update.Message.From.UserName, update.Message.Text)

		// Проверяем, является ли сообщение командой /start
		if update.Message.IsCommand() && update.Message.Command() == "start" {
			isRegistered, err := isUserRegistered(usersCollection, int(chatID))
			if err != nil {
				log.Printf("Ошибка проверки регистрации: %v", err)
				msg := tgbotapi.NewMessage(update.Message.Chat.ID, MSG_ERROR_OCCURRED)
				bot.Send(msg)
				continue
			}

			if isRegistered {
				msg := tgbotapi.NewMessage(update.Message.Chat.ID, MSG_START_REGISTERED)
				bot.Send(msg)
			} else {
				msg := tgbotapi.NewMessage(update.Message.Chat.ID, fmt.Sprintf(MSG_START_NOT_REGISTERED, MIN_APARTMENT_NUMBER, MAX_APARTMENT_NUMBER))
				bot.Send(msg)
			}
			continue
		}

		// Команда /addnews для администратора
		if update.Message.IsCommand() && update.Message.Command() == "addnews" {
			if chatID != ADMIN_ID {
				msg := tgbotapi.NewMessage(update.Message.Chat.ID, MSG_NO_PERMISSION)
				bot.Send(msg)
				continue
			}

			args := strings.TrimSpace(update.Message.CommandArguments())
			parts := strings.SplitN(args, " ", 2)
			if len(parts) != 2 {
				msg := tgbotapi.NewMessage(update.Message.Chat.ID, MSG_ADDNEWS_USAGE)
				bot.Send(msg)
				continue
			}

			title := parts[0]
			content := parts[1]

			err := addNews(newsCollection, title, content)
			if err != nil {
				log.Printf("Ошибка добавления новости: %v", err)
				msg := tgbotapi.NewMessage(update.Message.Chat.ID, MSG_ERROR_OCCURRED)
				bot.Send(msg)
				continue
			}

			msg := tgbotapi.NewMessage(update.Message.Chat.ID, MSG_NEWS_ADDED)
			bot.Send(msg)
			continue
		}

		// Команда /sendnews для отправки новостей всем пользователям
		if update.Message.IsCommand() && update.Message.Command() == "sendnews" {
			if chatID != ADMIN_ID {
				msg := tgbotapi.NewMessage(update.Message.Chat.ID, MSG_NO_PERMISSION)
				bot.Send(msg)
				continue
			}

			sendNewsToUsers(bot, usersCollection, newsCollection)
			msg := tgbotapi.NewMessage(update.Message.Chat.ID, MSG_NEWS_SEND_SUCCESS)
			bot.Send(msg)
			continue
		}

		// Команда /remind для отправки напоминаний о показаниях
		if update.Message.IsCommand() && update.Message.Command() == "remind" {
			if chatID != ADMIN_ID {
				msg := tgbotapi.NewMessage(update.Message.Chat.ID, MSG_NO_PERMISSION)
				bot.Send(msg)
				continue
			}

			checkAndSendReminders(bot, usersCollection, readingsCollection)
			msg := tgbotapi.NewMessage(update.Message.Chat.ID, MSG_REMINDERS_SENT)
			bot.Send(msg)
			continue
		}

		// Команда /history для получения истории показаний
		if update.Message.IsCommand() && update.Message.Command() == "history" {
			// Находим номер квартиры пользователя
			var user struct {
				ApartmentNumber int `bson:"apartment_number"`
			}

			err := usersCollection.FindOne(context.TODO(), bson.M{"chat_id": int(chatID)}).Decode(&user)
			if err != nil {
				if err == mongo.ErrNoDocuments {
					msg := tgbotapi.NewMessage(update.Message.Chat.ID, "Вы не зарегистрированы. Введите /start для начала.")
					bot.Send(msg)
					continue
				}
				log.Printf("Ошибка получения номера квартиры: %v", err)
				msg := tgbotapi.NewMessage(update.Message.Chat.ID, MSG_ERROR_OCCURRED)
				bot.Send(msg)
				continue
			}

			history, err := getHistory(readingsCollection, user.ApartmentNumber)
			if err != nil {
				log.Printf("Ошибка получения истории показаний: %v", err)
				msg := tgbotapi.NewMessage(update.Message.Chat.ID, MSG_ERROR_OCCURRED)
				bot.Send(msg)
				continue
			}

			msg := tgbotapi.NewMessage(update.Message.Chat.ID, history)
			bot.Send(msg)
			continue
		}

		// Если это не команда, обрабатываем текстовые сообщения
		if !update.Message.IsCommand() {
			// Проверяем, зарегистрирован ли пользователь
			isRegistered, err := isUserRegistered(usersCollection, int(chatID))
			if err != nil {
				log.Printf("Ошибка проверки регистрации: %v", err)
				msg := tgbotapi.NewMessage(update.Message.Chat.ID, MSG_ERROR_OCCURRED)
				bot.Send(msg)
				continue
			}

			if !isRegistered {
				// Регистрация нового пользователя
				apartmentNumberStr := strings.TrimSpace(update.Message.Text)
				apartmentNumber, err := strconv.Atoi(apartmentNumberStr)
				if err != nil || apartmentNumber < MIN_APARTMENT_NUMBER || apartmentNumber > MAX_APARTMENT_NUMBER {
					msg := tgbotapi.NewMessage(update.Message.Chat.ID, fmt.Sprintf(MSG_INVALID_APARTMENT, MIN_APARTMENT_NUMBER, MAX_APARTMENT_NUMBER))
					bot.Send(msg)
					continue
				}

				err = registerUser(usersCollection, int(chatID), apartmentNumber)
				if err != nil {
					log.Printf("Ошибка регистрации: %v", err)
					msg := tgbotapi.NewMessage(update.Message.Chat.ID, MSG_ERROR_OCCURRED)
					bot.Send(msg)
					continue
				}

				msg := tgbotapi.NewMessage(update.Message.Chat.ID, fmt.Sprintf(MSG_REGISTRATION_SUCCESS, apartmentNumber))
				bot.Send(msg)
				msg = tgbotapi.NewMessage(update.Message.Chat.ID, MSG_START_REGISTERED)
				bot.Send(msg)
				continue
			}

			// Находим номер квартиры пользователя
			var user struct {
				ApartmentNumber int `bson:"apartment_number"`
			}

			err = usersCollection.FindOne(context.TODO(), bson.M{"chat_id": int(chatID)}).Decode(&user)
			if err != nil {
				log.Printf("Ошибка получения номера квартиры: %v", err)
				msg := tgbotapi.NewMessage(update.Message.Chat.ID, MSG_ERROR_OCCURRED)
				bot.Send(msg)
				continue
			}

			// Обработка показаний
			parts := strings.Split(update.Message.Text, " ")
			if len(parts) != 2 {
				msg := tgbotapi.NewMessage(update.Message.Chat.ID, MSG_INVALID_FORMAT)
				bot.Send(msg)
				continue
			}

			cold, err := strconv.ParseFloat(parts[0], 64)
			if err != nil || cold <= 0 {
				msg := tgbotapi.NewMessage(update.Message.Chat.ID, MSG_VALUE_MUST_BE_POSITIVE)
				bot.Send(msg)
				continue
			}

			hot, err := strconv.ParseFloat(parts[1], 64)
			if err != nil || hot <= 0 {
				msg := tgbotapi.NewMessage(update.Message.Chat.ID, MSG_VALUE_MUST_BE_POSITIVE)
				bot.Send(msg)
				continue
			}

			// Получаем последние показания для квартиры
			lastCold, lastHot, err := getLastReadings(readingsCollection, user.ApartmentNumber)
			if err != nil && err != mongo.ErrNoDocuments {
				log.Printf("Ошибка получения последних показаний: %v", err)
				msg := tgbotapi.NewMessage(update.Message.Chat.ID, MSG_ERROR_OCCURRED)
				bot.Send(msg)
				continue
			}

			// Проверяем, что новые показания больше или равны предыдущим
			if lastCold > 0 && cold < lastCold {
				msg := tgbotapi.NewMessage(update.Message.Chat.ID, fmt.Sprintf(MSG_COLD_DECREASED, cold, lastCold))
				bot.Send(msg)
				continue
			}

			if lastHot > 0 && hot < lastHot {
				msg := tgbotapi.NewMessage(update.Message.Chat.ID, fmt.Sprintf(MSG_HOT_DECREASED, hot, lastHot))
				bot.Send(msg)
				continue
			}

			// Проверяем разницу между новыми и предыдущими показаниями
			if lastCold > 0 && cold-lastCold > 15 {
				msg := tgbotapi.NewMessage(update.Message.Chat.ID, fmt.Sprintf(MSG_LARGE_DIFFERENCE_COLD, cold-lastCold))
				bot.Send(msg)
				continue
			}

			if lastHot > 0 && hot-lastHot > 15 {
				msg := tgbotapi.NewMessage(update.Message.Chat.ID, fmt.Sprintf(MSG_LARGE_DIFFERENCE_HOT, hot-lastHot))
				bot.Send(msg)
				continue
			}

			// Сохраняем показания вместе с номером квартиры
			receivedTime, err := saveReadings(readingsCollection, user.ApartmentNumber, cold, hot)
			if err != nil {
				log.Printf("Ошибка сохранения показаний: %v", err)
				msg := tgbotapi.NewMessage(update.Message.Chat.ID, MSG_ERROR_OCCURRED)
				bot.Send(msg)
				continue
			}

			// Отправляем сообщение с временем приема
			msg := tgbotapi.NewMessage(update.Message.Chat.ID, fmt.Sprintf(MSG_READINGS_SUCCESS, cold, hot, receivedTime))
			bot.Send(msg)
		}
	}
}
