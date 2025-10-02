package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	_ "github.com/mattn/go-sqlite3"
	"github.com/mdp/qrterminal/v3"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
)

// Variabel ini akan diambil dari Environment Variables di Render
var NEXTJS_WEBHOOK_URL = os.Getenv("NEXTJS_WEBHOOK_URL")

type WhatsAppClient struct {
	Client *whatsmeow.Client
}

// eventHandler menangani event yang masuk dari WhatsApp
func (wh *WhatsAppClient) eventHandler(evt interface{}) {
	switch v := evt.(type) {
	case *events.Message:
		// Hanya proses pesan teks dan abaikan pesan dari bot itu sendiri atau grup
		if v.Info.IsFromMe || v.Message.GetConversation() == "" {
			return
		}

		sender := v.Info.Sender.String()
		message := v.Message.GetConversation()
		fmt.Printf("Pesan diterima dari %s: %s\n", sender, message)

		// Kirim data pesan ke backend API
		err := sendToWebhook(sender, message)
		if err != nil {
			log.Printf("Gagal memproses via webhook: %v\n", err)
			// Anda bisa menambahkan logika untuk mengirim pesan balasan error jika diperlukan
		}
	}
}

// sendToWebhook mengirimkan data pesan ke endpoint API Next.js
func sendToWebhook(sender, message string) error {
	if NEXTJS_WEBHOOK_URL == "" {
		return fmt.Errorf("NEXTJS_WEBHOOK_URL environment variable not set")
	}

	payload := map[string]string{
		"sender":  sender,
		"message": message,
	}
	jsonPayload, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("error marshalling payload: %w", err)
	}

	resp, err := http.Post(NEXTJS_WEBHOOK_URL, "application/json", bytes.NewBuffer(jsonPayload))
	if err != nil {
		return fmt.Errorf("error sending request to webhook: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("webhook returned non-200 status code: %d", resp.StatusCode)
	}

	fmt.Println("Pesan berhasil dikirim ke webhook backend.")
	return nil
}

func main() {
	dbLog := waLog.Stdout("Database", "INFO", true)
	// Membuat container store menggunakan SQLite
	// Path /data/ akan di-mount ke Persistent Disk di Render
	container, err := sqlstore.New("sqlite3", "file:/data/wa-session.db?_foreign_keys=on", dbLog)
	if err != nil {
		panic(err)
	}

	// Jika Anda ingin melihat log debug, ganti waLog.INFO dengan waLog.DEBUG
	clientLog := waLog.Stdout("Client", "INFO", true)
	client := whatsmeow.NewClient(container, clientLog)

	wh := &WhatsAppClient{Client: client}
	client.AddEventHandler(wh.eventHandler)

	if client.Store.ID == nil {
		// Belum login, perlu scan QR code
		qrChan, _ := client.GetQRChannel(context.Background())
		err = client.Connect()
		if err != nil {
			panic(err)
		}
		for evt := range qrChan {
			if evt.Event == "code" {
				fmt.Println("QR code diterima, scan dengan ponsel Anda:")
				qrterminal.Generate(evt.Code, qrterminal.L, os.Stdout)
				fmt.Println("Silakan scan QR code di atas untuk login.")
			} else {
				fmt.Println("Event login:", evt.Event)
			}
		}
	} else {
		// Sudah login, langsung konek
		fmt.Println("Sesi ditemukan, mencoba menghubungkan kembali...")
		err = client.Connect()
		if err != nil {
			panic(err)
		}
	}

	fmt.Println("Sudah login. Menunggu pesan masuk...")

	// Menunggu sinyal interrupt (Ctrl+C) untuk disconnect secara bersih
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	<-c

	client.Disconnect()
}
