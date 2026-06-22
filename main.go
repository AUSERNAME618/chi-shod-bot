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

const bufferSize = 5000

type CircularBuffer struct {
	data  [bufferSize]string
	index int
	count int
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
					"برای خلاصه گرفتن:\n"+
					"/chishod 100 — خلاصه ۱۰۰ پیام آخر\n"+
					"/chishod 500 — خلاصه ۵۰۰ پیام آخر\n\n"+
					fmt.Sprintf("📦 ظرفیت حافظه: %d پیام", bufferSize))
			botAPI.Send(msg)

		case strings.HasPrefix(text, "/chishod"):
			arg := text
			arg = strings.TrimPrefix(arg, "/chishod@"+botUsername)
			arg = strings.TrimPrefix(arg, "/chishod")
			arg = strings.TrimSpace(arg)

			if arg == "" {
				msg := tgbotapi.NewMessage(chatID,
					"لطفاً تعداد پیام رو مشخص کن.\nمثال: /chishod 200")
				botAPI.Send(msg)
				continue
			}

			n, parseErr := strconv.Atoi(arg)
			if parseErr != nil || n <= 0 {
				msg := tgbotapi.NewMessage(chatID,
					"عدد معتبر نیست.\nمثال: /chishod 200")
				botAPI.Send(msg)
				continue
			}

			if n > bufferSize {
				n = bufferSize
			}

			prompt, actualCount := cb.BuildPrompt(n)
			if prompt == "" {
				msg := tgbotapi.NewMessage(chatID, "هنوز پیامی در حافظه ثبت نشده.")
				botAPI.Send(msg)
				continue
			}

			botAPI.Send(tgbotapi.NewChatAction(chatID, tgbotapi.ChatTyping))

			summary := GroqRequest(groqToken, prompt)

			header := fmt.Sprintf("📋 *خلاصه %d پیام اخیر:*\n\n", actualCount)
			msg := tgbotapi.NewMessage(chatID, header+summary)
			msg.ParseMode = "Markdown"
			botAPI.Send(msg)

			// ❌ بافر پاک نمی‌شه — حافظه حفظ میشه

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

func (cb *CircularBuffer) BuildPrompt(n int) (string, int) {
	if cb.count == 0 {
		return "", 0
	}

	take := cb.count
	if n > 0 && n < cb.count {
		take = n
	}

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

	prompt := "این مکالمه از یک گروه تلگرامی فارسی‌زبانه. دو بخش بنویس:\n\n" +
		"📌 خلاصه کلی:\n" +
		"همه موضوعاتی که مطرح شده رو پوشش بده — هیچ‌چیزی رو حذف نکن. " +
		"اگه ۸۰ بحث بود، همه ۸۰ تا رو ذکر کن. طبیعی و روان بنویس، مثل کسی که برای یه دوست تعریف می‌کنه چی شد.\n\n" +
		"👤 خلاصه هر نفر:\n" +
		"برای هر کاربری که پیام داده، یه پاراگراف کوتاه اما واقعی بنویس — چه حرفایی زد، " +
		"چه موضوعاتی مطرح کرد، چه موضعی داشت. فقط چیزی که واقعاً گفته رو بنویس، نه کلیشه. " +
		"فرمت: «نام: ...\n»\n\n" +
		"مکالمه:\n" + strings.Join(lines, "\n")

	return prompt, len(lines)
}

func GroqRequest(token string, text string) string {
	config := openai.DefaultConfig(token)
	config.BaseURL = "https://api.groq.com/openai/v1"
	client := openai.NewClientWithConfig(config)

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
			MaxTokens: 4000,
		},
	)

	if err != nil {
		return fmt.Sprintf("❌ خطا: %v", err)
	}

	if len(resp.Choices) == 0 {
		return "❌ پاسخی دریافت نشد."
	}

	return resp.Choices[0].Message.Content
}