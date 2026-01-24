# 1. Базовый образ Go
FROM golang:1.21-alpine

# 2. Рабочая директория
WORKDIR /app

# 3. Копируем зависимости
COPY go.mod go.sum ./
RUN go mod download

# 4. Копируем код проекта
COPY . .

# 5. Сборка приложения
RUN go build -o bot main.go

# 6. Команда запуска
CMD ["./bot"]
