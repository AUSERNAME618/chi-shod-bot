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
	count int // تعداد واقعی پیام‌های ذخیره‌شده
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

	// Polling mode
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
				"سلام! 👋\n\n"+
					"دستورات:\n"+
					"/chishod — خلاصه همه پیام‌های بافر (تا ۱۰۰۰)\n"+
					"/chishod 200 — خلاصه ۲۰۰ پیام آخر\n"+
					"/chishod 500 — خلاصه ۵۰۰ پیام آخر")
			botAPI.Send(msg)

		case strings.HasPrefix(text, "/chishod"):
			// استخراج عدد از دستور (مثلاً /chishod 500 یا /chishod500)
			arg := text
			arg = strings.TrimPrefix(arg, "/chishod@"+botUsername)
			arg = strings.TrimPrefix(arg, "/chishod")
			arg = strings.TrimSpace(arg)

			requestedCount := 0 // 0 = همه پیام‌ها

			if arg != "" {
				n, parseErr := strconv.Atoi(arg)
				if parseErr != nil {
					msg := tgbotapi.NewMessage(chatID,
						"❌ فرمت اشتباه!\nمثال: /chishod یا /chishod 500")
					botAPI.Send(msg)
					continue
				}
				if n <= 0 {
					msg := tgbotapi.NewMessage(chatID, "❌ عدد باید بیشتر از ۰ باشه!")
					botAPI.Send(msg)
					continue
				}
				// سقف ۱۰۰۰ پیام
				if n > bufferSize {
					n = bufferSize
				}
				requestedCount = n
			}

			prompt, actualCount := cb.BuildPrompt(requestedCount)
			if prompt == "" {
				msg := tgbotapi.NewMessage(chatID, "❌ هنوز پیامی در بافر ثبت نشده!")
				botAPI.Send(msg)
				continue
			}

			botAPI.Send(tgbotapi.NewChatAction(chatID, tgbotapi.ChatTyping))

			summary := GroqRequest(groqToken, prompt)

			header := fmt.Sprintf("📋 *خلاصه %d پیام اخیر:*\n\n", actualCount)
			msg := tgbotapi.NewMessage(chatID, header+summary)
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
	if cb.count < bufferSize {
		cb.count++
	}
}

// BuildPrompt آخرین n پیام رو برمیگردونه (n=0 یعنی همه)
// همچنین تعداد واقعی پیام‌های انتخاب‌شده رو برمیگردونه
func (cb *CircularBuffer) BuildPrompt(n int) (string, int) {
	if cb.count == 0 {
		return "", 0
	}

	// تعیین تعداد
	take := cb.count
	if n > 0 && n < cb.count {
		take = n
	}

	// استخراج آخرین take پیام از بافر دایره‌ای
	start := (cb.index - take + bufferSize) % bufferSize
	var lines []string
	for i := 0; i < take; i++ {
		msg := cb.data[(start+i)%bufferSize]
		if msg != "" {
			lines = append(lines, msg)
		}
	}

	if len(lines) == 0 {
		return "", 0
	}

	prompt := "این یک مکالمه در یک گروه تلگرامی است. لطفاً دو بخش جداگانه به فارسی بنویس:\n\n" +
		"📌 خلاصه کلی (حداکثر ۳ خط): مهم‌ترین موضوعاتی که در گروه مطرح شده را بنویس.\n\n" +
		"👤 خلاصه هر عضو: برای هر کاربری که در مکالمه شرکت داشته، یک خط کوتاه بنویس که مهم‌ترین چیزی که گفته را توضیح دهد. فرمت دقیق: «نام: خلاصه»\n\n" +
		"مکالمه:\n" + strings.Join(lines, "\n")

	return prompt, len(lines)
}

func (cb *CircularBuffer) Empty() {
	for i := range cb.data {
		cb.data[i] = ""
	}
	cb.index = 0
	cb.count = 0
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
			MaxTokens: 1200,
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