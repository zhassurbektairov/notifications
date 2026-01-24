package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/joho/godotenv"
	"google.golang.org/api/option"
	"google.golang.org/api/sheets/v4"
)

// --- КОНФИГУРАЦИЯ ---
const SpreadsheetID = "1TiB-811WjRvkKYKCv6Wf-zz8J9MRxL3bzLIYosML6Cc"
const SheetBooking = "Booking"
const SheetUsers = "Users"

const DaysRange = "B1:H1"
const TimesRange = "A2:A10"
const DataStartRow = 2
const DataEndRow = 10

// --- СТРУКТУРЫ ДАННЫХ ---

type UserState int

const (
	StateNone UserState = iota
	StateWaitingName
	StateWaitingStudentID
)

type UserSession struct {
	State     UserState
	TempName  string
	RealName  string
	StudentID string
}

var (
	srv      *sheets.Service
	sessions = make(map[int64]*UserSession)
	mu       sync.Mutex
)

func main() {
	if err := godotenv.Load(); err != nil {
		log.Println("Инфо: файл .env не найден, используем системные переменные")
	}

	ctx := context.Background()

	credsJSON := os.Getenv("GOOGLE_CREDENTIALS")
	if credsJSON == "" {
		log.Fatal("ОШИБКА: Переменная окружения GOOGLE_CREDENTIALS пуста!")
	}

	var err error
	srv, err = sheets.NewService(ctx, option.WithCredentialsJSON([]byte(credsJSON)))
	if err != nil {
		log.Fatalf("Ошибка API Sheets: %v", err)
	}

	botToken := os.Getenv("API_TOKEN")
	if botToken == "" {
		log.Fatal("ОШИБКА: Переменная API_TOKEN пуста!")
	}

	bot, err := tgbotapi.NewBotAPI(botToken)
	if err != nil {
		log.Panic(err)
	}
	bot.Debug = true
	log.Printf("Бот %s запущен", bot.Self.UserName)

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60
	updates := bot.GetUpdatesChan(u)

	for update := range updates {
		if update.CallbackQuery != nil {
			handleCallback(bot, update.CallbackQuery)
			continue
		}
		if update.Message != nil {
			handleMessage(bot, update.Message)
		}
	}
}

// --- ЛОГИКА СООБЩЕНИЙ ---

func handleMessage(bot *tgbotapi.BotAPI, msg *tgbotapi.Message) {
	chatID := msg.Chat.ID
	tgID := msg.From.ID
	text := strings.TrimSpace(msg.Text)

	session := getSession(tgID)

	// Команда /start
	if text == "/start" {
		session.State = StateNone

		name, sid, found := checkUserInDB(tgID)
		if found {
			session.RealName = name
			session.StudentID = sid
			sendHTML(bot, chatID, fmt.Sprintf("👋 Hello, <b>%s</b>!", name))
			sendDaySelection(bot, chatID)
		} else {
			session.State = StateWaitingName
			sendHTML(bot, chatID, "👋 Welcome!\nPlease, write your <b>Name and Surname</b>:")
		}
		return
	}

	// Команда /my
	if text == "/my" {
		if session.RealName == "" || session.StudentID == "" {
			name, sid, found := checkUserInDB(tgID)
			if !found {
				sendHTML(bot, chatID, "First, write /start for registration.")
				return
			}
			session.RealName = name
			session.StudentID = sid
		}
		sendMySlots(bot, chatID, session.RealName, session.StudentID)
		return
	}

	// Машина состояний (Регистрация)
	switch session.State {
	case StateWaitingName:
		session.TempName = text
		session.State = StateWaitingStudentID
		sendHTML(bot, chatID, "Please, write your <b>Student ID</b>:")

	case StateWaitingStudentID:
		studentID := text
		saveUserToDB(tgID, session.TempName, studentID)

		session.RealName = session.TempName
		session.StudentID = studentID
		session.State = StateNone

		sendHTML(bot, chatID, "✅ Registration successful!")
		sendDaySelection(bot, chatID)

	default:
		if session.RealName != "" {
			sendHTML(bot, chatID, "Click -> /start.")
		}
	}
}

// --- ЛОГИКА КНОПОК ---

func handleCallback(bot *tgbotapi.BotAPI, cb *tgbotapi.CallbackQuery) {
	bot.Request(tgbotapi.NewCallback(cb.ID, ""))

	session := getSession(cb.From.ID)
	if session.RealName == "" {
		name, sid, found := checkUserInDB(cb.From.ID)
		if !found {
			sendHTML(bot, cb.Message.Chat.ID, "Authentication error. Click -> /start")
			return
		}
		session.RealName = name
		session.StudentID = sid
	}

	data := cb.Data
	chatID := cb.Message.Chat.ID
	msgID := cb.Message.MessageID

	// 1. Выбор дня
	if strings.HasPrefix(data, "day_") {
		colIdx, _ := strconv.Atoi(strings.TrimPrefix(data, "day_"))
		sendTimeSelection(bot, chatID, msgID, colIdx)
		return
	}

	// 2. Бронирование
	if strings.HasPrefix(data, "book_") {
		parts := strings.Split(data, "_")
		if len(parts) < 3 {
			return
		}
		colIdx, _ := strconv.Atoi(parts[1])
		rowIdx, _ := strconv.Atoi(parts[2])

		uniqueName := fmt.Sprintf("%s (%s)", session.RealName, session.StudentID)

		success, msg := bookSlot(colIdx, rowIdx, uniqueName)
		if success {
			successKb := tgbotapi.NewInlineKeyboardMarkup(
				tgbotapi.NewInlineKeyboardRow(
					tgbotapi.NewInlineKeyboardButtonData("📋 My Slots", "show_my_slots"),
					tgbotapi.NewInlineKeyboardButtonData("➕ Book another", "add_slot"),
				),
			)
			editHTML(bot, chatID, msgID, msg, &successKb)
		} else {
			bot.Request(tgbotapi.NewCallbackWithAlert(cb.ID, msg))
			sendTimeSelection(bot, chatID, msgID, colIdx)
		}
		return
	}

	// 3. Удаление
	if strings.HasPrefix(data, "del_") {
		cellA1 := strings.TrimPrefix(data, "del_")
		deleteSlot(cellA1)
		editHTML(bot, chatID, msgID, "🗑 Slot deleted!", nil)
		sendMySlots(bot, chatID, session.RealName, session.StudentID)
		return
	}

	if data == "add_slot" {
		bot.Send(tgbotapi.NewDeleteMessage(chatID, msgID))
		sendDaySelection(bot, chatID)
		return
	}

	if data == "back_days" {
		bot.Send(tgbotapi.NewDeleteMessage(chatID, msgID))
		sendDaySelection(bot, chatID)
		return
	}

	if data == "show_my_slots" {
		bot.Send(tgbotapi.NewDeleteMessage(chatID, msgID))
		sendMySlots(bot, chatID, session.RealName, session.StudentID)
		return
	}
}

// --- ФУНКЦИИ ИНТЕРФЕЙСА ---

func sendDaySelection(bot *tgbotapi.BotAPI, chatID int64) {
	resp, err := srv.Spreadsheets.Values.Get(SpreadsheetID, fmt.Sprintf("%s!%s", SheetBooking, DaysRange)).Do()
	if err != nil || len(resp.Values) == 0 {
		sendHTML(bot, chatID, "Error with Spreadsheet (days).")
		log.Printf("Error reading days: %v", err)
		return
	}

	days := resp.Values[0]
	var rows [][]tgbotapi.InlineKeyboardButton
	var currentRow []tgbotapi.InlineKeyboardButton

	for i, dayRaw := range days {
		dayName := fmt.Sprintf("%v", dayRaw)
		// ColIndex = i + 2 (A=1, Days start at B=2)
		btn := tgbotapi.NewInlineKeyboardButtonData(dayName, fmt.Sprintf("day_%d", i+2))
		currentRow = append(currentRow, btn)

		if len(currentRow) == 2 {
			rows = append(rows, currentRow)
			currentRow = []tgbotapi.InlineKeyboardButton{}
		}
	}
	if len(currentRow) > 0 {
		rows = append(rows, currentRow)
	}

	msg := tgbotapi.NewMessage(chatID, "📅 <b>Choose a day:</b>")
	msg.ParseMode = tgbotapi.ModeHTML
	msg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(rows...)
	bot.Send(msg)
}

func sendTimeSelection(bot *tgbotapi.BotAPI, chatID int64, msgID int, colIdx int) {
	colLetter := getColumnLetter(colIdx)

	respTime, err1 := srv.Spreadsheets.Values.Get(SpreadsheetID, fmt.Sprintf("%s!%s", SheetBooking, TimesRange)).Do()
	rangeSlots := fmt.Sprintf("%s!%s%d:%s%d", SheetBooking, colLetter, DataStartRow, colLetter, DataEndRow)
	respSlots, err2 := srv.Spreadsheets.Values.Get(SpreadsheetID, rangeSlots).Do()

	if err1 != nil || err2 != nil {
		log.Printf("Error reading time/slots: %v / %v", err1, err2)
		return
	}

	respDayName, _ := srv.Spreadsheets.Values.Get(SpreadsheetID, fmt.Sprintf("%s!%s1", SheetBooking, colLetter)).Do()
	dayName := "Day"
	if len(respDayName.Values) > 0 {
		dayName = fmt.Sprintf("%v", respDayName.Values[0][0])
	}

	var rows [][]tgbotapi.InlineKeyboardButton
	var currentRow []tgbotapi.InlineKeyboardButton

	// 9 слотов (индексы 0..8)
	for i := 0; i < 9; i++ {
		timeLabel := "Time"
		if i < len(respTime.Values) && len(respTime.Values[i]) > 0 {
			timeLabel = fmt.Sprintf("%v", respTime.Values[i][0])
		}

		isOccupied := false
		if i < len(respSlots.Values) && len(respSlots.Values[i]) > 0 {
			val := fmt.Sprintf("%v", respSlots.Values[i][0])
			if val != "" {
				isOccupied = true
			}
		}

		if !isOccupied {
			btn := tgbotapi.NewInlineKeyboardButtonData(timeLabel, fmt.Sprintf("book_%d_%d", colIdx, i+DataStartRow))
			currentRow = append(currentRow, btn)
			if len(currentRow) == 2 {
				rows = append(rows, currentRow)
				currentRow = []tgbotapi.InlineKeyboardButton{}
			}
		}
	}
	if len(currentRow) > 0 {
		rows = append(rows, currentRow)
	}

	rows = append(rows, tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("🔙 Back", "back_days")))

	text := fmt.Sprintf("🗓 <b>%s</b>\nChoose a time:", dayName)
	if len(rows) == 1 { // Только кнопка назад
		text += "\n😔 No slots available."
	}

	kb := tgbotapi.NewInlineKeyboardMarkup(rows...)
	editHTML(bot, chatID, msgID, text, &kb)
}

func sendMySlots(bot *tgbotapi.BotAPI, chatID int64, name, sid string) {
	uniqueName := fmt.Sprintf("%s (%s)", name, sid)

	readRange := fmt.Sprintf("%s!A1:H10", SheetBooking)
	resp, err := srv.Spreadsheets.Values.Get(SpreadsheetID, readRange).Do()
	if err != nil {
		sendHTML(bot, chatID, "Error with db.")
		return
	}

	data := resp.Values // data[row][col]
	var msgText strings.Builder
	msgText.WriteString(fmt.Sprintf("👤 <b>%s</b>\n📋 <b>Your slots:</b>\n\n", uniqueName))

	var rows [][]tgbotapi.InlineKeyboardButton
	foundCount := 0

	if len(data) > 0 {
		maxCols := len(data[0])

		for c := 1; c < maxCols; c++ {
			for r := 1; r < len(data); r++ {

				if c >= len(data[r]) {
					continue
				}

				val := fmt.Sprintf("%v", data[r][c])

				if strings.TrimSpace(val) == uniqueName {
					foundCount++

					dayName := "Day"
					if len(data[0]) > c {
						dayName = fmt.Sprintf("%v", data[0][c])
					}

					timeLabel := "Time"
					if len(data[r]) > 0 {
						timeLabel = fmt.Sprintf("%v", data[r][0])
					}

					msgText.WriteString(fmt.Sprintf("%d. <b>%s</b>: %s\n", foundCount, dayName, timeLabel))

					colLetter := getColumnLetter(c + 1)
					cellA1 := fmt.Sprintf("%s%d", colLetter, r+1)

					btnText := fmt.Sprintf("❌ Delete №%d (%s)", foundCount, dayName)
					rows = append(rows, tgbotapi.NewInlineKeyboardRow(
						tgbotapi.NewInlineKeyboardButtonData(btnText, "del_"+cellA1),
					))
				}
			}
		}
	}

	if foundCount == 0 {
		msgText.WriteString("You have no active slots.")
	}

	rows = append(rows, tgbotapi.NewInlineKeyboardRow(
		tgbotapi.NewInlineKeyboardButtonData("➕ Book another slot", "add_slot"),
	))

	msg := tgbotapi.NewMessage(chatID, msgText.String())
	msg.ParseMode = tgbotapi.ModeHTML
	msg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(rows...)
	bot.Send(msg)
}

// --- ВСПОМОГАТЕЛЬНЫЕ ДЛЯ HTML ---

func sendHTML(bot *tgbotapi.BotAPI, chatID int64, text string) {
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = tgbotapi.ModeHTML
	bot.Send(msg)
}

func editHTML(bot *tgbotapi.BotAPI, chatID int64, msgID int, text string, kb *tgbotapi.InlineKeyboardMarkup) {
	edit := tgbotapi.NewEditMessageText(chatID, msgID, text)
	edit.ParseMode = tgbotapi.ModeHTML
	if kb != nil {
		edit.ReplyMarkup = kb
	}
	bot.Send(edit)
}

// --- GOOGLE SHEETS & DB ---

func checkUserInDB(tgID int64) (string, string, bool) {
	resp, err := srv.Spreadsheets.Values.Get(SpreadsheetID, fmt.Sprintf("%s!A:C", SheetUsers)).Do()
	if err != nil {
		log.Printf("Error checking DB: %v", err)
		return "", "", false
	}
	tgIDStr := strconv.FormatInt(tgID, 10)
	for _, row := range resp.Values {
		if len(row) > 2 {
			if fmt.Sprintf("%v", row[0]) == tgIDStr {
				return fmt.Sprintf("%v", row[1]), fmt.Sprintf("%v", row[2]), true
			}
		}
	}
	return "", "", false
}

func saveUserToDB(tgID int64, name string, studentID string) {
	val := &sheets.ValueRange{
		Values: [][]interface{}{{tgID, name, studentID}},
	}
	_, err := srv.Spreadsheets.Values.Append(SpreadsheetID, fmt.Sprintf("%s!A1", SheetUsers), val).ValueInputOption("USER_ENTERED").Do()
	if err != nil {
		log.Printf("Error saving user: %v", err)
	}
}

func bookSlot(colIdx, rowIdx int, uniqueName string) (bool, string) {
	colLetter := getColumnLetter(colIdx)
	cell := fmt.Sprintf("%s!%s%d", SheetBooking, colLetter, rowIdx)

	resp, _ := srv.Spreadsheets.Values.Get(SpreadsheetID, cell).Do()
	if len(resp.Values) > 0 && len(resp.Values[0]) > 0 && fmt.Sprintf("%v", resp.Values[0][0]) != "" {
		return false, "⚠️ Slot is already taken!"
	}

	val := &sheets.ValueRange{Values: [][]interface{}{{uniqueName}}}
	_, err := srv.Spreadsheets.Values.Update(SpreadsheetID, cell, val).ValueInputOption("RAW").Do()
	if err != nil {
		log.Printf("Booking error: %v", err)
		return false, "Error"
	}
	return true, "✅ Booking is successful! Check your slots - /my."
}

func deleteSlot(cellA1 string) {
	val := &sheets.ValueRange{Values: [][]interface{}{{""}}}
	_, err := srv.Spreadsheets.Values.Update(SpreadsheetID, fmt.Sprintf("%s!%s", SheetBooking, cellA1), val).ValueInputOption("RAW").Do()
	if err != nil {
		log.Printf("Delete error: %v", err)
	}
}

// --- УТИЛИТЫ ---

func getSession(tgID int64) *UserSession {
	mu.Lock()
	defer mu.Unlock()
	if sessions[tgID] == nil {
		sessions[tgID] = &UserSession{State: StateNone}
	}
	return sessions[tgID]
}

func getColumnLetter(colIndex int) string {
	return string(rune('A' + colIndex - 1))
}
