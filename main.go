package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api"
	"github.com/joho/godotenv"
	"github.com/sashabaranov/go-openai"
)

const bufferSize = 1000

type CircularBuffer struct {
	data  [bufferSize]string
	index int
}

func main() {
	_ = godotenv.Load()

	botToken := os.Getenv("TELEGRAM_BOT_TOKEN")
	botUsername := os.Getenv("TELEGRAM_BOT_USERNAME")
	groqToken := os.Getenv("GROQ_API_TOKEN")
	groupChatIDStr := os.Getenv("TELEGRAM_GROUP_CHAT_ID")

	if botToken == "" || groqToken == "" || groupChatIDStr == "" {
		log.Fatal("Missing env vars: TELEGRAM_BOT_TOKEN, GROQ_API_TOKEN, TELEGRAM_GROUP_CHAT_ID")
	}

	groupChatID, err := strconv.ParseInt(groupChatIDStr, 10, 64)
	if err != nil {
		log.Fatal("TELEGRAM_GROUP_CHAT_ID must be a valid integer (e.g. -1001234567890)")
	}

	botAPI, err := tgbotapi.NewBotAPI(botToken)
	if err != nil {
		log.Panic(err)
	}
	botAPI.Debug = false
	log.Printf("✅ Bot running as @%s", botAPI.Self.UserName)

	// Health endpoint — keeps Render alive when pinged by UptimeRobot
	go func() {
		port := os.Getenv("PORT")
		if port == "" {
			port = "8080"
		}
		http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, "chi-shod-bot is alive ✅")
		})
		log.Printf("Health server on :%s", port)
		http.ListenAndServe(":"+port, nil)
	}()

	// Polling mode — no webhook / HTTPS required
	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60
	updates, err := botAPI.GetUpdatesChan(u)
	if err != nil {
		log.Panic(err)
	}

	cb := &CircularBuffer{}

	for update := range updates {
		if update.Message == nil {
			continue
		}

		chatID := update.Message.Chat.ID
		text := update.Message.Text

		if chatID != groupChatID {
			continue
		}

		switch {
		case text == "/start" || text == "/start@"+botUsername:
			msg := tgbotapi.NewMessage(chatID,
				"سلام! 👋\nبا دستور /chishod خلاصه‌ی آخرین پیام‌های گروه رو می‌گیری.")
			botAPI.Send(msg)

		case text == "/chishod" || text == "/chishod@"+botUsername:
			allMessages := cb.ConcatMessages()
			if allMessages == "" {
				msg := tgbotapi.NewMessage(chatID, "❌ هنوز پیامی در بافر ثبت نشده!")
				botAPI.Send(msg)
				continue
			}

			botAPI.Send(tgbotapi.NewChatAction(chatID, tgbotapi.ChatTyping))

			summary := GroqRequest(groqToken, allMessages)
			msg := tgbotapi.NewMessage(chatID, "📋 *خلاصه مکالمات اخیر:*\n\n"+summary)
			msg.ParseMode = "Markdown"
			botAPI.Send(msg)
			cb.Empty()

		default:
			cb.AddMessage(update.Message)
		}
	}
}

func (cb *CircularBuffer) AddMessage(message *tgbotapi.Message) {
	if len(strings.TrimSpace(message.Text)) <= 1 {
		return
	}
	text := message.From.FirstName
	if message.ReplyToMessage != nil {
		text += " (در پاسخ به " + message.ReplyToMessage.From.FirstName + ")"
	}
	text += ": " + strings.ReplaceAll(message.Text, "\n", " ")

	cb.data[cb.index] = text
	cb.index = (cb.index + 1) % bufferSize
}

func (cb *CircularBuffer) ConcatMessages() string {
	var messages []string
	messages = append(messages,
		"این یک مکالمه در یک گروه تلگرامی است. "+
			"یک خلاصه‌ی کوتاه (حداکثر ۳ خط) از مهم‌ترین موضوعات مطرح‌شده را به فارسی بنویس:")

	for i := 0; i < bufferSize; i++ {
		idx := (cb.index + i) % bufferSize
		if cb.data[idx] != "" {
			messages = append(messages, cb.data[idx])
		}
	}

	if len(messages) <= 1 {
		return ""
	}
	return strings.Join(messages, "\n")
}

func (cb *CircularBuffer) Empty() {
	for i := range cb.data {
		cb.data[i] = ""
	}
	cb.index = 0
}

func TrimToMax(s string, maxLen int) string {
	if len(s) > maxLen {
		return s[:maxLen]
	}
	return s
}

func GroqRequest(token string, text string) string {
	config := openai.DefaultConfig(token)
	config.BaseURL = "https://api.groq.com/openai/v1"
	client := openai.NewClientWithConfig(config)

	text = TrimToMax(text, 14000)

	resp, err := client.CreateChatCompletion(
		context.Background(),
		openai.ChatCompletionRequest{
			Model: "llama-3.3-70b-versatile",
			Messages: []openai.ChatCompletionMessage{
				{
					Role:    openai.ChatMessageRoleUser,
					Content: text,
				},
			},
			MaxTokens: 600,
		},
	)
	if err != nil {
		return fmt.Sprintf("❌ خطا در دریافت خلاصه: %v", err)
	}
	if len(resp.Choices) == 0 {
		return "❌ پاسخی دریافت نشد."
	}
	return resp.Choices[0].Message.Content
}