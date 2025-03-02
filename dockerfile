# Используем официальный Go образ как базовый
FROM golang:1.20 AS builder

# Устанавливаем рабочую директорию внутри контейнера
WORKDIR /app

# Копируем go.mod и go.sum файлы в рабочую директорию
COPY go.mod go.sum ./

# Загружаем зависимости
RUN go mod download

# Копируем исходный код в рабочую директорию
COPY . .

# Сборка приложения
RUN CGO_ENABLED=0 GOOS=linux go build -o bot ./main.go

# Создаем минимальный образ для запуска приложения
FROM alpine:latest

# Устанавливаем рабочую директорию внутри контейнера
WORKDIR /root/

# Копируем скомпилированное приложение из builder стадии
COPY --from=builder /app/bot .

# Указываем команду для запуска приложения
CMD ["./bot"]