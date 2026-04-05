package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/mdp/qrterminal/v3"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	_ "modernc.org/sqlite"
)

// ─────────────────────────────────────────────
// Config
// ─────────────────────────────────────────────

type Config struct {
	NextJSWebhookURL string
	DBPath           string
	HTTPPort         string
}

func LoadConfig() *Config {
	webhookURL := os.Getenv("NEXTJS_WEBHOOK_URL")
	if webhookURL == "" {
		webhookURL = "https://gotek.vercel.app/api/whatsapp-webhook"
		fmt.Printf("PERINGATAN: NEXTJS_WEBHOOK_URL tidak diset, menggunakan default: %s\n", webhookURL)
	} else {
		fmt.Printf("NEXTJS_WEBHOOK_URL = %s\n", webhookURL)
	}
	dbPath := os.Getenv("DB_PATH")
	if dbPath == "" {
		dbPath = "wa-session.db"
		fmt.Printf("PERINGATAN: DB_PATH tidak diset, menggunakan default: %s\n", dbPath)
	} else {
		fmt.Printf("DB_PATH = %s\n", dbPath)
	}
	if !filepath.IsAbs(dbPath) {
		if abs, err := filepath.Abs(dbPath); err == nil {
			dbPath = abs
		}
	}
	httpPort := os.Getenv("HTTP_PORT")
	if httpPort == "" {
		httpPort = "8080"
	}
	return &Config{
		NextJSWebhookURL: webhookURL,
		DBPath:           dbPath,
		HTTPPort:         httpPort,
	}
}

// ─────────────────────────────────────────────
// Prometheus Metrics
// ─────────────────────────────────────────────

type Metrics struct {
	// === 📨 Messaging ===
	MessagesReceived *prometheus.CounterVec  // labels: source, type
	MessagesSent     *prometheus.CounterVec  // labels: status
	MessagesIgnored  *prometheus.CounterVec  // labels: reason
	ImagesProcessed  *prometheus.CounterVec  // labels: status
	ActiveSenders    prometheus.Counter

	// === 🌐 Webhook & Latency ===
	WebhookRequests *prometheus.CounterVec   // labels: type, status
	WebhookDuration *prometheus.HistogramVec // labels: type
	WebhookRespSize *prometheus.HistogramVec // labels: type
	WebhookErrors   *prometheus.CounterVec   // labels: reason

	// === 📡 HTTP API ===
	HTTPRequests       *prometheus.CounterVec   // labels: endpoint, method, status_code
	HTTPDuration       *prometheus.HistogramVec // labels: endpoint
	BroadcastJobs      *prometheus.CounterVec   // labels: status
	BroadcastMsgSent   prometheus.Counter
	BroadcastMsgFailed prometheus.Counter
	BroadcastActive    prometheus.Gauge

	// === 🔌 Connection & Health ===
	WAConnected              prometheus.Gauge
	ReconnectAttempts        prometheus.Counter
	LastMsgReceivedTimestamp prometheus.Gauge
	LastMsgSentTimestamp     prometheus.Gauge
	UptimeSeconds            prometheus.Gauge

	startTime time.Time
}

func newMetrics() *Metrics {
	m := &Metrics{startTime: time.Now()}

	m.MessagesReceived = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "wabot_messages_received_total",
		Help: "Total pesan masuk (label: source=private/group, type=text/image)",
	}, []string{"source", "type"})

	m.MessagesSent = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "wabot_messages_sent_total",
		Help: "Total pesan balasan yang dikirim bot (label: status=success/error)",
	}, []string{"status"})

	m.MessagesIgnored = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "wabot_messages_ignored_total",
		Help: "Total pesan yang diabaikan bot (label: reason=not_tagged/from_self)",
	}, []string{"reason"})

	m.ImagesProcessed = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "wabot_image_processed_total",
		Help: "Total gambar/struk yang diproses AI (label: status=success/error)",
	}, []string{"status"})

	m.ActiveSenders = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "wabot_active_senders_total",
		Help: "Jumlah unique sender yang pernah mengirim pesan (monotonic)",
	})

	m.WebhookRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "wabot_webhook_requests_total",
		Help: "Total request ke Next.js webhook (label: type=text/image, status=success/error)",
	}, []string{"type", "status"})

	m.WebhookDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "wabot_webhook_duration_seconds",
		Help:    "Durasi round-trip ke Next.js webhook dalam detik",
		Buckets: []float64{0.1, 0.5, 1, 2, 5, 10, 15, 20, 30},
	}, []string{"type"})

	m.WebhookRespSize = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "wabot_webhook_response_size_bytes",
		Help:    "Ukuran response body dari Next.js webhook dalam bytes",
		Buckets: []float64{100, 500, 1000, 5000, 10000, 50000},
	}, []string{"type"})

	m.WebhookErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "wabot_webhook_errors_total",
		Help: "Breakdown tipe error webhook (label: reason=timeout/non200/unmarshal/request_failed/read_error)",
	}, []string{"reason"})

	m.HTTPRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "wabot_http_requests_total",
		Help: "Total request ke HTTP API bot (label: endpoint, method, status_code)",
	}, []string{"endpoint", "method", "status_code"})

	m.HTTPDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "wabot_http_duration_seconds",
		Help:    "Latency handler HTTP API bot dalam detik",
		Buckets: []float64{0.01, 0.05, 0.1, 0.5, 1, 5},
	}, []string{"endpoint"})

	m.BroadcastJobs = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "wabot_broadcast_jobs_total",
		Help: "Total broadcast job yang dimulai",
	}, []string{"status"})

	m.BroadcastMsgSent = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "wabot_broadcast_messages_sent_total",
		Help: "Total individu pesan dalam broadcast yang berhasil terkirim",
	})

	m.BroadcastMsgFailed = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "wabot_broadcast_messages_failed_total",
		Help: "Total individu pesan dalam broadcast yang gagal",
	})

	m.BroadcastActive = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "wabot_broadcast_active",
		Help: "Jumlah broadcast job yang sedang berjalan saat ini",
	})

	m.WAConnected = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "wabot_whatsapp_connected",
		Help: "Status koneksi WhatsApp (1=connected, 0=disconnected)",
	})

	m.ReconnectAttempts = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "wabot_reconnect_attempts_total",
		Help: "Jumlah kali bot mencoba reconnect ke WhatsApp",
	})

	m.LastMsgReceivedTimestamp = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "wabot_last_message_received_timestamp",
		Help: "Unix timestamp pesan terakhir yang diterima (gunakan untuk deteksi bot stuck)",
	})

	m.LastMsgSentTimestamp = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "wabot_last_message_sent_timestamp",
		Help: "Unix timestamp pesan terakhir yang berhasil dikirim bot",
	})

	m.UptimeSeconds = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "wabot_uptime_seconds",
		Help: "Berapa lama bot sudah berjalan dalam detik",
	})

	prometheus.MustRegister(
		m.MessagesReceived,
		m.MessagesSent,
		m.MessagesIgnored,
		m.ImagesProcessed,
		m.ActiveSenders,
		m.WebhookRequests,
		m.WebhookDuration,
		m.WebhookRespSize,
		m.WebhookErrors,
		m.HTTPRequests,
		m.HTTPDuration,
		m.BroadcastJobs,
		m.BroadcastMsgSent,
		m.BroadcastMsgFailed,
		m.BroadcastActive,
		m.WAConnected,
		m.ReconnectAttempts,
		m.LastMsgReceivedTimestamp,
		m.LastMsgSentTimestamp,
		m.UptimeSeconds,
	)

	return m
}

// ─────────────────────────────────────────────
// Handler
// ─────────────────────────────────────────────

type Handler struct {
	Client      *whatsmeow.Client
	Config      *Config
	HTTP        *http.Client
	Metrics     *Metrics
	seenSenders sync.Map // set untuk track unique senders
}

// ─────────────────────────────────────────────
// Request / Response types
// ─────────────────────────────────────────────

type SendMessageRequest struct {
	Phone   string `json:"phone"`
	Message string `json:"message"`
}

type SendMessageResponse struct {
	Success bool   `json:"success"`
	Error   string `json:"error,omitempty"`
}

type BroadcastRequest struct {
	Phones       []string `json:"phones"`
	Message      string   `json:"message"`
	DelaySeconds int      `json:"delay_seconds"`
}

type BroadcastResponse struct {
	Success bool   `json:"success"`
	Status  string `json:"status"`
	Total   int    `json:"total"`
	Error   string `json:"error,omitempty"`
}

// ─────────────────────────────────────────────
// HTTP Instrumentation Helpers
// ─────────────────────────────────────────────

// responseRecorder menangkap status code HTTP untuk metrics
type responseRecorder struct {
	http.ResponseWriter
	statusCode int
}

func newResponseRecorder(w http.ResponseWriter) *responseRecorder {
	return &responseRecorder{ResponseWriter: w, statusCode: http.StatusOK}
}

func (rr *responseRecorder) WriteHeader(code int) {
	rr.statusCode = code
	rr.ResponseWriter.WriteHeader(code)
}

// instrumentedHandler membungkus HandlerFunc dengan timing dan counter metrics
func (h *Handler) instrumentedHandler(endpoint string, fn http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rr := newResponseRecorder(w)
		fn(rr, r)
		h.Metrics.HTTPRequests.WithLabelValues(endpoint, r.Method, fmt.Sprintf("%d", rr.statusCode)).Inc()
		h.Metrics.HTTPDuration.WithLabelValues(endpoint).Observe(time.Since(start).Seconds())
	}
}

// isTimeoutError mengecek apakah error adalah jenis timeout
func isTimeoutError(err error) bool {
	s := err.Error()
	return strings.Contains(s, "timeout") ||
		strings.Contains(s, "deadline exceeded") ||
		strings.Contains(s, "context deadline")
}

// ─────────────────────────────────────────────
// Event Handler
// ─────────────────────────────────────────────

func (h *Handler) eventHandler(evt interface{}) {
	switch v := evt.(type) {

	case *events.Connected:
		h.Metrics.WAConnected.Set(1)
		log.Println("✅ WhatsApp terhubung!")

	case *events.Disconnected:
		h.Metrics.WAConnected.Set(0)
		log.Println("⚠️ WhatsApp terputus!")

	case *events.Message:
		// Skip pesan dari diri sendiri
		if v.Info.IsFromMe {
			h.Metrics.MessagesIgnored.WithLabelValues("from_self").Inc()
			return
		}

		recipientJID := v.Info.Chat.ToNonAD()
		chatString := v.Info.Chat.ToNonAD().String()
		senderString := v.Info.Sender.ToNonAD().String()
		if senderString == "" || senderString == "unknown@s.whatsapp.net" {
			senderString = chatString
		}

		// Track unique senders
		if _, loaded := h.seenSenders.LoadOrStore(senderString, true); !loaded {
			h.Metrics.ActiveSenders.Inc()
		}

		source := "private"
		if v.Info.Chat.Server == "g.us" {
			source = "group"
		}

		// === Unwrap message containers ===
		msg := v.Message
		if ephemeral := msg.GetEphemeralMessage(); ephemeral != nil {
			if inner := ephemeral.GetMessage(); inner != nil {
				fmt.Printf("📦 Unwrapped EphemeralMessage from %s\n", senderString)
				msg = inner
			}
		}
		if viewOnce := msg.GetViewOnceMessage(); viewOnce != nil {
			if inner := viewOnce.GetMessage(); inner != nil {
				fmt.Printf("📦 Unwrapped ViewOnceMessage from %s\n", senderString)
				msg = inner
			}
		}
		if viewOnceV2 := msg.GetViewOnceMessageV2(); viewOnceV2 != nil {
			if inner := viewOnceV2.GetMessage(); inner != nil {
				fmt.Printf("📦 Unwrapped ViewOnceMessageV2 from %s\n", senderString)
				msg = inner
			}
		}

		// === Handle Image Messages ===
		if imgMsg := msg.GetImageMessage(); imgMsg != nil {
			caption := imgMsg.GetCaption()
			mimetype := imgMsg.GetMimetype()
			fmt.Printf("📸 Gambar diterima dari %s (mimetype: %s, caption: %s)\n", senderString, mimetype, caption)

			if v.Info.Chat.Server == "g.us" {
				lowerCaption := strings.ToLower(caption)
				if !strings.Contains(lowerCaption, "@gotek") &&
					!strings.Contains(lowerCaption, "@bot") &&
					!strings.Contains(lowerCaption, "@66190395355362") &&
					!strings.Contains(lowerCaption, "@asisten") {
					fmt.Printf("🔇 [GRUP] Gambar diabaikan (bot tidak di-tag di caption): %s\n", caption)
					h.Metrics.MessagesIgnored.WithLabelValues("not_tagged").Inc()
					return
				}
			}

			h.Metrics.MessagesReceived.WithLabelValues(source, "image").Inc()
			h.Metrics.LastMsgReceivedTimestamp.SetToCurrentTime()

			processingMsg := "⏳ *Memproses struk...*\n\nSedang menganalisis gambar dengan AI. Mohon tunggu sebentar."
			h.Client.SendMessage(context.Background(), recipientJID, &waProto.Message{
				Conversation: &processingMsg,
			})

			imageData, err := h.Client.Download(context.Background(), imgMsg)
			if err != nil {
				log.Printf("❌ Gagal download gambar: %v", err)
				errMsg := "❌ Gagal mengunduh gambar. Coba kirim ulang."
				h.Client.SendMessage(context.Background(), recipientJID, &waProto.Message{
					Conversation: &errMsg,
				})
				h.Metrics.ImagesProcessed.WithLabelValues("error").Inc()
				return
			}

			base64Image := base64.StdEncoding.EncodeToString(imageData)
			fmt.Printf("📸 Gambar berhasil didownload (%d bytes, base64: %d chars)\n", len(imageData), len(base64Image))

			replyMessage, err := h.sendImageToWebhook(senderString, chatString, base64Image, mimetype, caption)
			if err != nil {
				log.Printf("❌ Gagal memproses gambar via webhook: %v\n", err)
				replyMessage = "❌ Gagal memproses struk. Coba lagi nanti."
				h.Metrics.ImagesProcessed.WithLabelValues("error").Inc()
			} else {
				h.Metrics.ImagesProcessed.WithLabelValues("success").Inc()
			}

			if replyMessage != "" {
				_, err := h.Client.SendMessage(context.Background(), recipientJID, &waProto.Message{
					Conversation: &replyMessage,
				})
				if err != nil {
					log.Printf("❌ Gagal mengirim balasan gambar: %v", err)
					h.Metrics.MessagesSent.WithLabelValues("error").Inc()
				} else {
					fmt.Printf("✅ Balasan struk terkirim ke %s\n", senderString)
					h.Metrics.MessagesSent.WithLabelValues("success").Inc()
					h.Metrics.LastMsgSentTimestamp.SetToCurrentTime()
				}
			}
			return
		}

		// Debug log
		fmt.Printf("🔍 DEBUG Message from %s - HasConversation:%v HasExtended:%v HasImage:%v HasVideo:%v HasDocument:%v HasEphemeral:%v HasViewOnce:%v\n",
			senderString,
			v.Message.GetConversation() != "",
			v.Message.GetExtendedTextMessage() != nil,
			v.Message.GetImageMessage() != nil,
			v.Message.GetVideoMessage() != nil,
			v.Message.GetDocumentMessage() != nil,
			v.Message.GetEphemeralMessage() != nil,
			v.Message.GetViewOnceMessage() != nil,
		)

		// === Handle Text Messages ===
		message := msg.GetConversation()
		if message == "" {
			if extMsg := msg.GetExtendedTextMessage(); extMsg != nil {
				message = extMsg.GetText()
			}
		}
		if message == "" {
			if btnResp := msg.GetButtonsResponseMessage(); btnResp != nil {
				message = btnResp.GetSelectedDisplayText()
			}
		}
		if message == "" {
			if listResp := msg.GetListResponseMessage(); listResp != nil {
				message = listResp.GetTitle()
			}
		}
		if message == "" {
			fmt.Printf("⚠️ Pesan diterima tapi tidak bisa di-extract dari %s. MessageType: %T\n",
				v.Info.Sender.User, v.Message)
			return
		}

		fmt.Printf("📩 Pesan diterima dari %s (chat: %s): %s\n", senderString, chatString, message)
		h.Metrics.MessagesReceived.WithLabelValues(source, "text").Inc()
		h.Metrics.LastMsgReceivedTimestamp.SetToCurrentTime()

		// Group silent mode filter
		isGroup := v.Info.Chat.Server == "g.us"
		if isGroup {
			lowerMsg := strings.ToLower(message)
			isBotMentioned := strings.Contains(lowerMsg, "@gotek") ||
				strings.Contains(lowerMsg, "@bot") ||
				strings.Contains(lowerMsg, "@asisten") ||
				strings.Contains(lowerMsg, "@66190395355362")
			if !isBotMentioned {
				fmt.Printf("🔇 [GRUP] Pesan diabaikan (bot tidak di-tag): %s\n", message)
				h.Metrics.MessagesIgnored.WithLabelValues("not_tagged").Inc()
				return
			}
			fmt.Printf("📢 [GRUP] Bot di-tag, memproses pesan: %s\n", message)
		}

		replyMessage, err := h.sendToWebhook(senderString, chatString, message)
		if err != nil {
			log.Printf("❌ Gagal memproses via webhook: %v\n", err)
			replyMessage = "Maaf, terjadi kesalahan di server. Coba lagi nanti."
		}

		if replyMessage != "" {
			_, err := h.Client.SendMessage(context.Background(), recipientJID, &waProto.Message{
				Conversation: &replyMessage,
			})
			if err != nil {
				log.Printf("❌ Gagal mengirim pesan balasan: %v", err)
				h.Metrics.MessagesSent.WithLabelValues("error").Inc()
			} else {
				fmt.Printf("✅ Pesan balasan terkirim ke %s\n", senderString)
				h.Metrics.MessagesSent.WithLabelValues("success").Inc()
				h.Metrics.LastMsgSentTimestamp.SetToCurrentTime()
			}
		}
	}
}

// ─────────────────────────────────────────────
// Webhook Functions
// ─────────────────────────────────────────────

func (h *Handler) sendToWebhook(sender, chatId, message string) (string, error) {
	start := time.Now()

	payload := map[string]string{
		"sender":  sender,
		"chat_id": chatId,
		"message": message,
	}
	jsonPayload, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("error marshalling payload: %w", err)
	}

	resp, err := h.HTTP.Post(h.Config.NextJSWebhookURL, "application/json", bytes.NewBuffer(jsonPayload))
	if err != nil {
		h.Metrics.WebhookDuration.WithLabelValues("text").Observe(time.Since(start).Seconds())
		h.Metrics.WebhookRequests.WithLabelValues("text", "error").Inc()
		if isTimeoutError(err) {
			h.Metrics.WebhookErrors.WithLabelValues("timeout").Inc()
		} else {
			h.Metrics.WebhookErrors.WithLabelValues("request_failed").Inc()
		}
		return "", fmt.Errorf("error sending request to webhook: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		h.Metrics.WebhookDuration.WithLabelValues("text").Observe(time.Since(start).Seconds())
		h.Metrics.WebhookRequests.WithLabelValues("text", "error").Inc()
		h.Metrics.WebhookErrors.WithLabelValues("non200").Inc()
		return "", fmt.Errorf("webhook returned non-200 status code: %d", resp.StatusCode)
	}

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		h.Metrics.WebhookDuration.WithLabelValues("text").Observe(time.Since(start).Seconds())
		h.Metrics.WebhookRequests.WithLabelValues("text", "error").Inc()
		h.Metrics.WebhookErrors.WithLabelValues("read_error").Inc()
		return "", fmt.Errorf("error reading response body: %w", err)
	}

	var responseBody map[string]string
	if err := json.Unmarshal(bodyBytes, &responseBody); err != nil {
		h.Metrics.WebhookDuration.WithLabelValues("text").Observe(time.Since(start).Seconds())
		h.Metrics.WebhookRequests.WithLabelValues("text", "error").Inc()
		h.Metrics.WebhookErrors.WithLabelValues("unmarshal").Inc()
		return "", fmt.Errorf("error unmarshalling response body: %w", err)
	}

	h.Metrics.WebhookDuration.WithLabelValues("text").Observe(time.Since(start).Seconds())
	h.Metrics.WebhookRespSize.WithLabelValues("text").Observe(float64(len(bodyBytes)))
	h.Metrics.WebhookRequests.WithLabelValues("text", "success").Inc()
	fmt.Println("Pesan berhasil dikirim ke webhook backend.")
	return responseBody["message"], nil
}

func (h *Handler) sendImageToWebhook(sender, chatId, base64Image, mimetype, caption string) (string, error) {
	start := time.Now()

	payload := map[string]string{
		"sender":   sender,
		"chat_id":  chatId,
		"image":    base64Image,
		"mimetype": mimetype,
		"caption":  caption,
	}
	jsonPayload, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("error marshalling image payload: %w", err)
	}

	imageWebhookURL := strings.TrimSuffix(h.Config.NextJSWebhookURL, "/") + "/image"
	fmt.Printf("📸 Mengirim gambar ke webhook: %s (%d chars)\n", imageWebhookURL, len(base64Image))

	resp, err := h.HTTP.Post(imageWebhookURL, "application/json", bytes.NewBuffer(jsonPayload))
	if err != nil {
		h.Metrics.WebhookDuration.WithLabelValues("image").Observe(time.Since(start).Seconds())
		h.Metrics.WebhookRequests.WithLabelValues("image", "error").Inc()
		if isTimeoutError(err) {
			h.Metrics.WebhookErrors.WithLabelValues("timeout").Inc()
		} else {
			h.Metrics.WebhookErrors.WithLabelValues("request_failed").Inc()
		}
		return "", fmt.Errorf("error sending image to webhook: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		h.Metrics.WebhookDuration.WithLabelValues("image").Observe(time.Since(start).Seconds())
		h.Metrics.WebhookRequests.WithLabelValues("image", "error").Inc()
		h.Metrics.WebhookErrors.WithLabelValues("non200").Inc()
		return "", fmt.Errorf("image webhook returned status %d: %s", resp.StatusCode, string(bodyBytes))
	}

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		h.Metrics.WebhookDuration.WithLabelValues("image").Observe(time.Since(start).Seconds())
		h.Metrics.WebhookRequests.WithLabelValues("image", "error").Inc()
		h.Metrics.WebhookErrors.WithLabelValues("read_error").Inc()
		return "", fmt.Errorf("error reading image webhook response: %w", err)
	}

	var responseBody map[string]string
	if err := json.Unmarshal(bodyBytes, &responseBody); err != nil {
		h.Metrics.WebhookDuration.WithLabelValues("image").Observe(time.Since(start).Seconds())
		h.Metrics.WebhookRequests.WithLabelValues("image", "error").Inc()
		h.Metrics.WebhookErrors.WithLabelValues("unmarshal").Inc()
		return "", fmt.Errorf("error unmarshalling image webhook response: %w", err)
	}

	h.Metrics.WebhookDuration.WithLabelValues("image").Observe(time.Since(start).Seconds())
	h.Metrics.WebhookRespSize.WithLabelValues("image").Observe(float64(len(bodyBytes)))
	h.Metrics.WebhookRequests.WithLabelValues("image", "success").Inc()
	fmt.Println("📸 Gambar berhasil diproses oleh webhook backend.")
	return responseBody["message"], nil
}

// ─────────────────────────────────────────────
// HTTP Server
// ─────────────────────────────────────────────

func (h *Handler) setupHTTPServer() *http.ServeMux {
	mux := http.NewServeMux()

	// === /metrics — Prometheus scrape endpoint ===
	mux.Handle("/metrics", promhttp.Handler())

	// === /send-message ===
	mux.HandleFunc("/send-message", h.instrumentedHandler("/send-message", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.Method != "POST" {
			json.NewEncoder(w).Encode(SendMessageResponse{Success: false, Error: "Method not allowed"})
			return
		}
		var req SendMessageRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(SendMessageResponse{Success: false, Error: "Invalid JSON payload"})
			return
		}
		if req.Phone == "" || req.Message == "" {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(SendMessageResponse{Success: false, Error: "Phone and message are required"})
			return
		}
		phone := req.Phone
		if !strings.HasSuffix(phone, "@s.whatsapp.net") {
			phone = phone + "@s.whatsapp.net"
		}
		jid, err := types.ParseJID(phone)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(SendMessageResponse{Success: false, Error: fmt.Sprintf("Invalid phone number: %v", err)})
			return
		}
		_, err = h.Client.SendMessage(context.Background(), jid, &waProto.Message{
			Conversation: &req.Message,
		})
		if err != nil {
			log.Printf("Failed to send message to %s: %v", req.Phone, err)
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(SendMessageResponse{Success: false, Error: fmt.Sprintf("Failed to send message: %v", err)})
			return
		}
		log.Printf("✅ Message sent to %s via HTTP API", req.Phone)
		h.Metrics.MessagesSent.WithLabelValues("success").Inc()
		h.Metrics.LastMsgSentTimestamp.SetToCurrentTime()
		json.NewEncoder(w).Encode(SendMessageResponse{Success: true})
	}))

	// === /broadcast ===
	mux.HandleFunc("/broadcast", h.instrumentedHandler("/broadcast", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.Method != "POST" {
			w.WriteHeader(http.StatusMethodNotAllowed)
			json.NewEncoder(w).Encode(BroadcastResponse{Success: false, Error: "Method not allowed"})
			return
		}
		var req BroadcastRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(BroadcastResponse{Success: false, Error: "Invalid JSON"})
			return
		}
		if len(req.Phones) == 0 || req.Message == "" {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(BroadcastResponse{Success: false, Error: "Phones and message required"})
			return
		}
		delay := req.DelaySeconds
		if delay <= 0 {
			delay = 45
		}

		h.Metrics.BroadcastJobs.WithLabelValues("started").Inc()
		h.Metrics.BroadcastActive.Inc()

		go func() {
			defer h.Metrics.BroadcastActive.Dec()
			log.Printf("📢 Broadcast dimulai: %d penerima, delay %ds", len(req.Phones), delay)
			for i, phone := range req.Phones {
				p := phone
				if !strings.HasSuffix(p, "@s.whatsapp.net") {
					p = p + "@s.whatsapp.net"
				}
				jid, err := types.ParseJID(p)
				if err != nil {
					log.Printf("❌ Broadcast [%d/%d] invalid JID %s: %v", i+1, len(req.Phones), phone, err)
					h.Metrics.BroadcastMsgFailed.Inc()
					continue
				}
				msg := req.Message
				_, err = h.Client.SendMessage(context.Background(), jid, &waProto.Message{
					Conversation: &msg,
				})
				if err != nil {
					log.Printf("❌ Broadcast [%d/%d] gagal ke %s: %v", i+1, len(req.Phones), phone, err)
					h.Metrics.BroadcastMsgFailed.Inc()
				} else {
					log.Printf("✅ Broadcast [%d/%d] terkirim ke %s", i+1, len(req.Phones), phone)
					h.Metrics.BroadcastMsgSent.Inc()
					h.Metrics.LastMsgSentTimestamp.SetToCurrentTime()
				}
				if i < len(req.Phones)-1 {
					time.Sleep(time.Duration(delay) * time.Second)
				}
			}
			log.Printf("📢 Broadcast selesai")
		}()

		json.NewEncoder(w).Encode(BroadcastResponse{
			Success: true,
			Status:  "processing",
			Total:   len(req.Phones),
		})
	}))

	// === /health ===
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]bool{"ok": true})
	})

	return mux
}

func main() {
	cfg := LoadConfig()

	m := newMetrics()

	go func() {
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			m.UptimeSeconds.Set(time.Since(m.startTime).Seconds())
		}
	}()

	dbDir := filepath.Dir(cfg.DBPath)
	if err := os.MkdirAll(dbDir, 0755); err != nil {
		log.Fatalf("Gagal membuat direktori %s: %v", dbDir, err)
	}
	dbLog := waLog.Stdout("Database", "WARN", true)
	dsn := fmt.Sprintf("file:%s?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)", cfg.DBPath)
	container, err := sqlstore.New(context.Background(), "sqlite", dsn, dbLog)
	if err != nil {
		log.Fatalf("sqlstore.New error: %v\nDSN=%s", err, dsn)
	}
	deviceStore, err := container.GetFirstDevice(context.Background())
	if err != nil {
		log.Fatalf("GetFirstDevice error: %v", err)
	}
	if deviceStore == nil {
		log.Println("Tidak ada sesi tersimpan, membuat device baru...")
		deviceStore = container.NewDevice()
	}

	clientLog := waLog.Stdout("Client", "WARN", true)
	client := whatsmeow.NewClient(deviceStore, clientLog)

	h := &Handler{
		Client:  client,
		Config:  cfg,
		Metrics: m,
		HTTP: &http.Client{
			Timeout: 60 * time.Second,
		},
	}
	client.AddEventHandler(h.eventHandler)

	m.WAConnected.Set(0)

	if client.Store.ID == nil {
		qrChan, _ := client.GetQRChannel(context.Background())
		if err := client.Connect(); err != nil {
			log.Fatalf("client.Connect (first-time) error: %v", err)
		}
		m.ReconnectAttempts.Inc()
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
		m.ReconnectAttempts.Inc()
		if err := client.Connect(); err != nil {
			log.Fatalf("client.Connect (reconnect) error: %v", err)
		}
	}

	fmt.Println("Sudah login. Menunggu pesan masuk...")

	// Start HTTP server
	go func() {
		mux := h.setupHTTPServer()
		addr := ":" + cfg.HTTPPort
		fmt.Printf("🌐 HTTP Server listening on %s\n", addr)
		fmt.Printf("📊 Prometheus metrics tersedia di http://localhost%s/metrics\n", addr)
		if err := http.ListenAndServe(addr, mux); err != nil {
			log.Printf("HTTP Server error: %v", err)
		}
	}()

	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	<-c
	client.Disconnect()
}