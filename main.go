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

	_ "github.com/mattn/go-sqlite3"
	"github.com/mdp/qrterminal/v3"

	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/store/sqlstore"
	// "go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
)

var NEXTJS_WEBHOOK_URL = os.Getenv("NEXTJS_WEBHOOK_URL")

type WhatsAppClient struct {
	Client *whatsmeow.Client
}

// eventHandler sekarang memiliki kemampuan untuk mengirim balasan
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

		// Kirim data ke webhook dan dapatkan pesan balasan
		replyMessage, err := sendToWebhook(sender, message)
		if err != nil {
			log.Printf("Gagal memproses via webhook: %v\n", err)
			// Kirim pesan error umum jika webhook gagal
			replyMessage = "Maaf, terjadi kesalahan di server. Coba lagi nanti."
		}

		// Jika ada pesan balasan dari API, kirim kembali ke pengguna
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

// sendToWebhook sekarang mengembalikan pesan balasan dari API
func sendToWebhook(sender, message string) (string, error) {
	if NEXTJS_WEBHOOK_URL == "" {
		NEXTJS_WEBHOOK_URL = "http://localhost:3000/api/whatsapp-webhook"
		fmt.Println("PERINGATAN: NEXTJS_WEBHOOK_URL tidak diset, menggunakan default localhost.")
	}
	payload := map[string]string{
		"sender":  sender,
		"message": message,
	}
	jsonPayload, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("error marshalling payload: %w", err)
	}

	resp, err := http.Post(NEXTJS_WEBHOOK_URL, "application/json", bytes.NewBuffer(jsonPayload))
	if err != nil {
		return "", fmt.Errorf("error sending request to webhook: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("webhook returned non-200 status code: %d", resp.StatusCode)
	}

	// Baca dan parse pesan balasan dari body respons
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

func main() {
	dbLog := waLog.Stdout("Database", "INFO", true)

	dbPath := "wa-session.db"

	container, err := sqlstore.New(context.Background(), "sqlite3", fmt.Sprintf("file:%s?_foreign_keys=on", dbPath), dbLog)
	if err != nil {
		panic(err)
	}

	deviceStore, err := container.GetFirstDevice(context.Background())
	if err != nil {
		panic(err)
	}

	clientLog := waLog.Stdout("Client", "INFO", true)
	client := whatsmeow.NewClient(deviceStore, clientLog)

	wh := &WhatsAppClient{Client: client}
	client.AddEventHandler(wh.eventHandler)

	if client.Store.ID == nil {
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

