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
	"path/filepath"
	

	_ "net/http/pprof"

	_ "modernc.org/sqlite"
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
		// Gunakan ToNonAD() untuk mendapatkan JID tanpa device part
		recipientJID := senderJID.ToNonAD()
		sender := senderJID.String()
		message := v.Message.GetConversation()
		fmt.Printf("Pesan diterima dari %s: %s\n", sender, message)

		replyMessage, err := sendToWebhook(sender, message)
		if err != nil {
			log.Printf("Gagal memproses via webhook: %v\n", err)
			replyMessage = "Maaf, terjadi kesalahan di server. Coba lagi nanti."
		}

		if replyMessage != "" {
			_, err := wh.Client.SendMessage(context.Background(), recipientJID, &waProto.Message{
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
        // NEXTJS_WEBHOOK_URL = "https://fe-whatsapp-bot.vercel.app/api/whatsapp-webhook"
        NEXTJS_WEBHOOK_URL = "https://gotek.vercel.app/api/whatsapp-webhook"
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

	// gunakan env DB_PATH jika ada, kalau tidak pakai nilai dari init()/default
	dbPath := DB_PATH
	if dbPath == "" {
		log.Fatal("DB_PATH kosong — set environment variable DB_PATH ke path sqlite (contoh: C:\\data\\wa-session.db atau /var/www/wa-session.db)")
	}

	// jika path relatif terdeteksi, ubah ke absolute agar konsisten
	if !filepath.IsAbs(dbPath) {
		abs, err := filepath.Abs(dbPath)
		if err == nil {
			dbPath = abs
		}
	}

	// pastikan folder ada
	dbDir := filepath.Dir(dbPath)
	if _, err := os.Stat(dbDir); os.IsNotExist(err) {
		if err := os.MkdirAll(dbDir, 0755); err != nil {
			log.Fatalf("Gagal membuat direktori %s: %v", dbDir, err)
		}
	}

	// jika file belum ada, buat file kosong agar sqlite bisa buka
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		f, err := os.Create(dbPath)
		if err != nil {
			log.Fatalf("Gagal membuat file DB %s: %v", dbPath, err)
		}
		f.Close()
	}

	// sqlite DSN: pakai file:path?param=.. ; di Windows backslash tidak perlu di-escape khusus karena kita pakai filepath
	// modernc.org/sqlite menggunakan _pragma untuk set foreign_keys dan busy_timeout
	// busy_timeout=5000 = tunggu 5 detik jika database locked sebelum error
	dsn := fmt.Sprintf("file:%s?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)", dbPath)

	container, err := sqlstore.New(context.Background(), "sqlite", dsn, dbLog)
	if err != nil {
		log.Fatalf("sqlstore.New error: %v\nDSN=%s", err, dsn)
	}

	deviceStore, err := container.GetFirstDevice(context.Background())
	if err != nil {
		// tidak langsung panic — beri pesan yang jelas
		log.Fatalf("GetFirstDevice error: %v\nPastikan session sebelumnya tersimpan di DB atau jalankan flow QR login.", err)
	}

	clientLog := waLog.Stdout("Client", "WARN", true)
	client := whatsmeow.NewClient(deviceStore, clientLog)

	wh := &WhatsAppClient{Client: client}
	client.AddEventHandler(wh.eventHandler)

	go func() {
		fmt.Println("Server pprof berjalan di http://localhost:6060/debug/pprof/")
		log.Println(http.ListenAndServe("localhost:6060", nil))
	}()

	// Connect / login handling
	if client.Store == nil {
		log.Fatalf("client.Store == nil, tidak bisa melanjutkan")
	}

	if client.Store.ID == nil {
		qrChan, _ := client.GetQRChannel(context.Background())
		if err := client.Connect(); err != nil {
			log.Fatalf("client.Connect (first-time) error: %v", err)
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
		if err := client.Connect(); err != nil {
			log.Fatalf("client.Connect (reconnect) error: %v", err)
		}
	}

	fmt.Println("Sudah login. Menunggu pesan masuk...")

	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	<-c

	client.Disconnect()
}


