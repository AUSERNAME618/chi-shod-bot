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

const (
	bufferSize       = 5000
	telegramMaxChars = 4000
	aiModel          = "deepseek-ai/deepseek-v4-pro"
	aiBaseURL        = "https://integrate.api.nvidia.com/v1"
)

type CircularBuffer struct {
	data  [bufferSize]string
	index int
	count int
}

func main() {
	_ = godotenv.Load()

	botToken := os.Getenv("TELEGRAM_BOT_TOKEN")
	botUsername := os.Getenv("TELEGRAM_BOT_USERNAME")
	nvidiaToken := os.Getenv("NVIDIA_API_KEY")
	groupChatIDStr := os.Getenv("TELEGRAM_GROUP_CHAT_ID")

	if botToken == "" || nvidiaToken == "" || groupChatIDStr == "" {
		log.Fatal("Missing env vars: TELEGRAM_BOT_TOKEN, NVIDIA_API_KEY, TELEGRAM_GROUP_CHAT_ID")
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
					fmt.Sprintf("📦 ظرفیت حافظه: %d پیام (وقتی پر بشه، قدیمی‌ها خودکار جای جدیدها رو خالی می‌کنن)", bufferSize))
			botAPI.Send(msg)

		case strings.HasPrefix(text, "/chishod"):
			arg := text
			arg = strings.TrimPrefix(arg, "/chishod@"+botUsername)
			arg = strings.TrimPrefix(arg, "/chishod")
			arg = strings.TrimSpace(arg)

			if arg == "" {
				botAPI.Send(tgbotapi.NewMessage(chatID, "لطفاً تعداد پیام رو مشخص کن.\nمثال: /chishod 200"))
				continue
			}

			n, parseErr := strconv.Atoi(arg)
			if parseErr != nil || n <= 0 {
				botAPI.Send(tgbotapi.NewMessage(chatID, "عدد معتبر نیست.\nمثال: /chishod 200"))
				continue
			}

			lines, actualCount := cb.GetLines(n)
			if len(lines) == 0 {
				botAPI.Send(tgbotapi.NewMessage(chatID, "هنوز پیامی در حافظه ثبت نشده."))
				continue
			}

			botAPI.Send(tgbotapi.NewChatAction(chatID, tgbotapi.ChatTyping))

			summary, err := aiCall(nvidiaToken, buildPrompt(lines))
			if err != nil {
				botAPI.Send(tgbotapi.NewMessage(chatID, fmt.Sprintf("❌ خطا: %v", err)))
				continue
			}

			var header string
			if actualCount < n {
				header = fmt.Sprintf("📋 خلاصه %d پیام موجود (کمتر از %d درخواستی، چون هنوز این تعداد جمع نشده):\n\n", actualCount, n)
			} else {
				header = fmt.Sprintf("📋 خلاصه %d پیام اخیر:\n\n", actualCount)
			}

			SendLongMessage(botAPI, chatID, header+summary)

		default:
			cb.AddMessage(update.Message)
		}
	}
}

func (cb *CircularBuffer) AddMessage(message *tgbotapi.Message) {
	if message.From == nil || len(strings.TrimSpace(message.Text)) <= 1 {
		return
	}

	text := message.From.FirstName
	if message.ReplyToMessage != nil && message.ReplyToMessage.From != nil {
		text += " (در پاسخ به " + message.ReplyToMessage.From.FirstName + ")"
	}
	text += ": " + strings.ReplaceAll(message.Text, "\n", " ")

	cb.data[cb.index] = text
	cb.index = (cb.index + 1) % bufferSize
	if cb.count < bufferSize {
		cb.count++
	}
}

// GetLines دقیقاً n پیامِ آخر رو برمی‌گردونه؛ نه کمتر نه بیشتر
// (مگر اینکه هنوز اون تعداد پیام در حافظه جمع نشده باشه)
func (cb *CircularBuffer) GetLines(n int) ([]string, int) {
	if cb.count == 0 {
		return nil, 0
	}

	take := n
	if take > cb.count {
		take = cb.count
	}
	if take > bufferSize {
		take = bufferSize
	}

	start := (cb.index - take + bufferSize) % bufferSize
	lines := make([]string, 0, take)
	for i := 0; i < take; i++ {
		msg := cb.data[(start+i)%bufferSize]
		if msg != "" {
			lines = append(lines, msg)
		}
	}
	return lines, len(lines)
}

func buildPrompt(lines []string) string {
	return "این مکالمه از یک گروه تلگرامی فارسی‌زبانه. فقط همین پیام‌هایی که می‌دم رو خلاصه کن، نه کمتر نه بیشتر، و دو بخش جدا بنویس:\n\n" +
		"📌 خلاصه کلی:\n" +
		"همه موضوعات و بحث‌هایی که در همین بازه مطرح شده رو پوشش بده، هیچ‌کدوم رو حذف یا فراموش نکن — حتی اگه ده‌ها موضوع جدا از هم باشن. " +
		"طوری بنویس که کسی که اصلاً این پیام‌ها رو نخونده، بعد از خوندن خلاصه، دقیقاً بفهمه چه اتفاقی افتاده و چه بحث‌هایی شده، انگار خودش اونجا بوده. " +
		"طبیعی، روان و خودمونی بنویس، نه خشک و رباتیک.\n\n" +
		"👤 خلاصه هر نفر:\n" +
		"برای هر کاربری که پیام داده، فقط یک یا دو جمله‌ی کوتاه و مفید بنویس که خلاصه‌ی نقش و حرف‌های اون فرد باشه. " +
		"از تکرار چیزی که قبلاً توی خلاصه کلی گفتی پرهیز کن، فقط نکته‌ی خاص و متمایز هر نفر رو بگو. شلوغ و پراکنده ننویس.\n\n" +
		"مکالمه (دقیقاً همین تعداد پیام، نه کمتر نه بیشتر):\n" + strings.Join(lines, "\n")
}

func aiCall(token, prompt string) (string, error) {
	config := openai.DefaultConfig(token)
	config.BaseURL = aiBaseURL
	client := openai.NewClientWithConfig(config)

	resp, err := client.CreateChatCompletion(
		context.Background(),
		openai.ChatCompletionRequest{
			Model: aiModel,
			Messages: []openai.ChatCompletionMessage{
				{Role: openai.ChatMessageRoleUser, Content: prompt},
			},
			MaxTokens: 16000,
		},
	)
	if err != nil {
		return "", err
	}
	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("پاسخی دریافت نشد")
	}
	return resp.Choices[0].Message.Content, nil
}

// SendLongMessage پیام طولانی رو به چند تکه‌ی زیر ۴۰۹۶ کاراکتر (محدودیت خود تلگرام) تقسیم می‌کنه
func SendLongMessage(botAPI *tgbotapi.BotAPI, chatID int64, text string) {
	runes := []rune(text)
	for len(runes) > 0 {
		cut := telegramMaxChars
		if len(runes) < cut {
			cut = len(runes)
		} else {
			for i := cut - 1; i > 0; i-- {
				if runes[i] == '\n' {
					cut = i
					break
				}
			}
		}
		chunk := strings.TrimSpace(string(runes[:cut]))
		if chunk != "" {
			botAPI.Send(tgbotapi.NewMessage(chatID, chunk))
		}
		runes = runes[cut:]
	}
}