package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	_ "net/http/pprof"

	_ "github.com/mattn/go-sqlite3"
	"github.com/mdp/qrterminal/v3"

	"time"

	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
)

var httpClient = &http.Client{
    // Anda bisa mengatur Timeout di sini untuk menghindari request yang menggantung
    Timeout: 15 * time.Second, 
}
// var NEXTJS_WEBHOOK_URL = os.Getenv("NEXTJS_WEBHOOK_URL")
var NEXTJS_WEBHOOK_URL string

type WhatsAppClient struct {
	Client *whatsmeow.Client
}
var DB_PATH string

func (wh *WhatsAppClient) eventHandler(evt interface{}) {
	switch v := evt.(type) {
	case *events.Message:
		if v.Info.IsFromMe || v.Message.GetConversation() == "" {
			return
		}

		senderJID := v.Info.Sender
		sender := senderJID.String()
		message := v.Message.GetConversation()
		fmt.Printf("Pesan diterima dari %s: %s\n", sender, message)

		replyMessage, err := sendToWebhook(sender, message)
		if err != nil {
			log.Printf("Gagal memproses via webhook: %v\n", err)
			replyMessage = "Maaf, terjadi kesalahan di server. Coba lagi nanti."
		}

		if replyMessage != "" {
			_, err := wh.Client.SendMessage(context.Background(), senderJID, &waProto.Message{
				Conversation: &replyMessage,
			})
			if err != nil {
				log.Printf("Gagal mengirim pesan balasan: %v", err)
			} else {
				fmt.Printf("Pesan balasan terkirim ke %s\n", sender)
			}
		}
	}
}

func sendToWebhook(sender, message string) (string, error) {
	// if NEXTJS_WEBHOOK_URL == "" {
	// 	// NEXTJS_WEBHOOK_URL = "https://fe-whatsapp-bot.vercel.app/api/whatsapp-webhook"
	// 	NEXTJS_WEBHOOK_URL = "http://localhost:3000/api/whatsapp-webhook"
	// 	fmt.Println("PERINGATAN: NEXTJS_WEBHOOK_URL tidak diset, menggunakan default localhost.")
	// }
	payload := map[string]string{
		"sender":  sender,
		"message": message,
	}
	jsonPayload, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("error marshalling payload: %w", err)
	}

	resp, err := httpClient.Post(NEXTJS_WEBHOOK_URL, "application/json", bytes.NewBuffer(jsonPayload))
	if err != nil {
		return "", fmt.Errorf("error sending request to webhook: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("webhook returned non-200 status code: %d", resp.StatusCode)
	}

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("error reading response body: %w", err)
	}

	var responseBody map[string]string
	if err := json.Unmarshal(bodyBytes, &responseBody); err != nil {
		return "", fmt.Errorf("error unmarshalling response body: %w", err)
	}

	fmt.Println("Pesan berhasil dikirim ke webhook backend.")
	return responseBody["message"], nil
}

func init() {
    NEXTJS_WEBHOOK_URL = os.Getenv("NEXTJS_WEBHOOK_URL")
    if NEXTJS_WEBHOOK_URL == "" {
        NEXTJS_WEBHOOK_URL = "https://fe-whatsapp-bot.vercel.app/api/whatsapp-webhook"
        fmt.Println("PERINGATAN: NEXTJS_WEBHOOK_URL tidak diset, menggunakan default:", NEXTJS_WEBHOOK_URL)
    } else {
        fmt.Println("NEXTJS_WEBHOOK_URL =", NEXTJS_WEBHOOK_URL)
    }

    DB_PATH = os.Getenv("DB_PATH")
    if DB_PATH == "" {
        DB_PATH = "wa-session.db" // default relative (tapi sebaiknya set absolute via env)
        fmt.Println("PERINGATAN: DB_PATH tidak diset, menggunakan default:", DB_PATH)
    } else {
        fmt.Println("DB_PATH =", DB_PATH)
    }
}

func main() {
	dbLog := waLog.Stdout("Database", "WARN", true)

	dbPath := "/www/whatsapp-bot/wa-session.db"

	container, err := sqlstore.New(context.Background(), "sqlite3", fmt.Sprintf("file:%s?_foreign_keys=on", dbPath), dbLog)
	if err != nil {
		panic(err)
	}

	deviceStore, err := container.GetFirstDevice(context.Background())
	if err != nil {
		panic(err)
	}

	clientLog := waLog.Stdout("Client", "WARN", true)
	client := whatsmeow.NewClient(deviceStore, clientLog)

	wh := &WhatsAppClient{Client: client}
	client.AddEventHandler(wh.eventHandler)

	go func() {
        fmt.Println("Server pprof berjalan di http://localhost:6060/debug/pprof/")
        log.Println(http.ListenAndServe("localhost:6060", nil))
    }()

	if client.Store.ID == nil {
		qrChan, _ := client.GetQRChannel(context.Background())
		err = client.Connect()
		if err != nil {
			panic(err)
		}
		for evt := range qrChan {
			if evt.Event == "code" {
				fmt.Println("QR code diterima, scan dengan ponsel Anda:")

				config := qrterminal.Config{
					Level:      qrterminal.L,
					Writer:     os.Stdout,
					HalfBlocks: true,
				}
				qrterminal.GenerateWithConfig(evt.Code, config)

				fmt.Println("Silakan scan QR code di atas untuk login.")
			} else {
				fmt.Println("Event login:", evt.Event)
			}
		}
	} else {
		fmt.Println("Sesi ditemukan, mencoba menghubungkan kembali...")
		err = client.Connect()
		if err != nil {
			panic(err)
		}
	}

	fmt.Println("Sudah login. Menunggu pesan masuk...")

	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	<-c

	client.Disconnect()
}

