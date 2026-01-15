package main

import (
	"context"
	"encoding/json"
	"github.com/gorilla/websocket"
	"github.com/joho/godotenv"
	"github.com/redis/go-redis/v9"
	"log"
	"net/http"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Konfigurasi dari Environment Variables
var (
	websocketSecretKey string
	redisURL           string
	port               string
	redisPubPoolSize   string
	redisSubPoolSize   string
)

// --- Struct untuk Pesan JSON ---

// IncomingMessage mewakili pesan dari klien
type IncomingMessage struct {
	Action        string          `json:"action"`
	Channel       string          `json:"channel"`
	Data          json.RawMessage `json:"data"`
	MessageID     string          `json:"messageId"`
	LastMessageID string          `json:"lastMessageId"`
}

// OutgoingMessage mewakili pesan broadcast ke klien
type OutgoingMessage struct {
	Event    string      `json:"event"`
	Channel  string      `json:"channel"`
	StreamID string      `json:"streamId"`
	Data     interface{} `json:"data"`
}

// --- Struct untuk State Management ---

// Client adalah representasi koneksi WebSocket
// Kita butuh mutex untuk mencegah penulisan bersamaan ke koneksi
type Client struct {
	conn *websocket.Conn
	mu   sync.Mutex
}

// SendJSON adalah helper aman-konkurensi untuk menulis ke WebSocket
func (c *Client) SendJSON(v interface{}) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn.WriteJSON(v)
}

// Metrics melacak statistik server, mirip dengan objek metrics di JS
type Metrics struct {
	messagesIn          atomic.Uint64
	messagesOut         atomic.Uint64
	publishLatencyTotal atomic.Uint64 // Dlm nanosekon
	publishCount        atomic.Uint64
}

// Hub mengelola semua state server, klien, dan channel
type Hub struct {
	// Peta channel ke set klien
	// map[channelName]map[*Client]bool
	channels map[string]map[*Client]bool
	// Mutex untuk melindungi akses ke map 'channels'
	channelsMu sync.RWMutex

	// Set channel yang sedang memiliki real-time reader aktif
	activeStreamReaders map[string]bool
	// Mutex untuk melindungi akses ke map 'activeStreamReaders'
	readerMu sync.Mutex

	// Klien Redis untuk operasi reguler (XADD, XREAD histori)
	redisClient *redis.Client
	// Klien Redis terpisah untuk operasi blocking (XREAD BLOCK)
	subscriberClient *redis.Client

	isRedisReady atomic.Bool
	metrics      *Metrics
	upgrader     websocket.Upgrader
}

// NewHub membuat instance Hub baru
func NewHub() *Hub {
	hub := &Hub{
		channels:            make(map[string]map[*Client]bool),
		activeStreamReaders: make(map[string]bool),
		metrics:             &Metrics{},
		upgrader: websocket.Upgrader{
			ReadBufferSize:  1024,
			WriteBufferSize: 1024,
			CheckOrigin: func(r *http.Request) bool {
				// Izinkan semua origin untuk saat ini
				return true
			},
		},
	}

	hub.setupRedis()
	go hub.runMetricsLogger()

	return hub
}

// Konfigurasi Timeouts
const (
    pongWait   = 60 * time.Second
    pingPeriod = (pongWait * 9) / 10 // Harus kurang dari pongWait (misal 54 detik)
)

// setupRedis menginisialisasi dan memonitor koneksi Redis

func (h *Hub) setupRedis() {
	// --- 1. Persiapan Konfigurasi Pool ---

	// Parse PUB Pool Size
	pubPoolSizeInt, err := strconv.Atoi(redisPubPoolSize)
	if err != nil || pubPoolSizeInt <= 0 {
		log.Printf("⚠️ Format REDIS_PUB_POOL_SIZE salah ('%s'). Fallback ke 50.", redisPubPoolSize)
		pubPoolSizeInt = 50
	} else {
		log.Printf("⚙️ Konfigurasi: PUB Pool Size diatur ke %d", pubPoolSizeInt)
	}

	// Parse SUB Pool Size
	subPoolSizeInt, err := strconv.Atoi(redisSubPoolSize)
	if err != nil || subPoolSizeInt <= 0 {
		log.Printf("⚠️ Format REDIS_SUB_POOL_SIZE salah ('%s'). Fallback ke 50.", redisSubPoolSize)
		subPoolSizeInt = 50
	} else {
		log.Printf("⚙️ Konfigurasi: SUB Pool Size diatur ke %d", subPoolSizeInt)
	}

	// --- 2. Setup Klien PUB (Publisher) ---
	pubOpt, err := redis.ParseURL(redisURL)
	if err != nil {
		log.Fatalf("Gagal parse REDIS_URL (PUB): %v", err)
	}

	// Terapkan konfigurasi Pool Size
	pubOpt.PoolSize = pubPoolSizeInt
	pubOpt.PoolTimeout = 10 * time.Second // Standar

	h.redisClient = redis.NewClient(pubOpt)

	// --- 3. Setup Klien SUB (Subscriber / Blocking Reader) ---
	subOpt, err := redis.ParseURL(redisURL)
	if err != nil {
		log.Fatalf("Gagal parse REDIS_URL (SUB): %v", err)
	}

	// Terapkan konfigurasi Pool Size
	subOpt.PoolSize = subPoolSizeInt

	// Logika Cerdas: Jika Pool Size kecil (mode testing), kurangi Timeout agar error cepat muncul
	if subPoolSizeInt < 10 {
		subOpt.PoolTimeout = 2 * time.Second
		log.Println("⚙️ [SUB] Mode Testing: PoolTimeout dipendekkan ke 2 detik.")
	} else {
		subOpt.PoolTimeout = 10 * time.Second // Standar
	}

	h.subscriberClient = redis.NewClient(subOpt)

	// --- 4. Mulai Monitor ---
	go h.monitorRedisConnection(h.redisClient, "PUB")
	go h.monitorRedisConnection(h.subscriberClient, "SUB")
}

// monitorRedisConnection memeriksa koneksi secara berkala
func (h *Hub) monitorRedisConnection(client *redis.Client, name string) {
	ctx := context.Background()
	for {
		if err := client.Ping(ctx).Err(); err != nil {
			log.Printf("[REDIS-%s] Error: %v", name, err)
			if name == "PUB" {
				h.isRedisReady.Store(false)
			}
		} else {
			if !h.isRedisReady.Load() && name == "PUB" {
				log.Printf("✅ [REDIS-%s] Terhubung dan siap.", name)
				h.isRedisReady.Store(true)
			}
		}
		time.Sleep(5 * time.Second) // Ping setiap 5 detik
	}
}

// runMetricsLogger mencatat metrik setiap 10 detik
func (h *Hub) runMetricsLogger() {
	for {
		time.Sleep(10 * time.Second)

		// 1. Ambil Metrik Aplikasi
		in := h.metrics.messagesIn.Load()
		out := h.metrics.messagesOut.Load()
		count := h.metrics.publishCount.Load()
		latencyTotalNs := h.metrics.publishLatencyTotal.Load()

		var avgLatencyMs float64
		if count > 0 {
			avgLatencyMs = float64(latencyTotalNs) / float64(count) / float64(time.Millisecond)
		}

		// 2. Ambil Statistik Pool Redis
		pubStats := h.redisClient.PoolStats()
		subStats := h.subscriberClient.PoolStats()

		// Hitung koneksi yang sedang sibuk/terpakai
		pubUsed := pubStats.TotalConns - pubStats.IdleConns
		subUsed := subStats.TotalConns - subStats.IdleConns

		// 3. Hitung Active Channels (Real-time Readers) [BARU]
		// Kita butuh Lock karena map ini tidak thread-safe untuk dibaca saat ada yang nulis
		h.readerMu.Lock()
		activeChannelsCount := len(h.activeStreamReaders)
		h.readerMu.Unlock()

		log.Println("\n--- METRICS (10s) ---")
		log.Printf("Messages In (from Clients): %d", in)
		log.Printf("Messages Out (to Clients):  %d", out)
		log.Printf("Avg. Redis Publish Latency: %.2f ms", avgLatencyMs)

		// Log Info Channel [BARU]
		// Ini menunjukkan berapa banyak goroutine reader yang 'HIDUP'
		log.Printf("Active Channels (Readers):  %d", activeChannelsCount)

		log.Println("- Redis Pool Stats -")
		log.Printf("PUB Client: %d Used / %d Total (Idle: %d, Misses: %d)",
			pubUsed, pubStats.TotalConns, pubStats.IdleConns, pubStats.Misses)
		// Perhatikan perbandingan antara 'Active Channels' dengan 'SUB Client Used' di bawah ini
		log.Printf("SUB Client: %d Used / %d Total (Idle: %d, Misses: %d)",
			subUsed, subStats.TotalConns, subStats.IdleConns, subStats.Misses)
		log.Println("-----------------------\n")

		// Reset nilai metrik aplikasi
		h.metrics.messagesIn.Store(0)
		h.metrics.messagesOut.Store(0)
		h.metrics.publishCount.Store(0)
		h.metrics.publishLatencyTotal.Store(0)
	}
}

// --- Logika Inti WebSocket (Porting dari server.js) ---

// handleWebSocket adalah handler http untuk koneksi baru
func (h *Hub) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	// 1. Autentikasi
	clientToken := r.URL.Query().Get("token")
	if clientToken != websocketSecretKey {
		log.Println("[AUTH] Koneksi ditolak: Secret key salah.")
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	// 2. Upgrade koneksi
	conn, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("[WSS] Gagal upgrade koneksi: %v", err)
		return
	}
	log.Println("✅ [WSS] Klien terhubung dengan sukses.")

	client := &Client{conn: conn}

	// Kirim pesan selamat datang
	client.SendJSON(map[string]string{"status": "connected", "message": "Welcome!"})

	// 3. Mulai read-loop untuk klien ini
	// handleClientMessages akan berjalan sampai koneksi ditutup
	h.handleClientMessages(client)
}

// handleClientMessages (menggantikan ws.on('message'))
func (h *Hub) handleClientMessages(client *Client) {
	client.conn.SetReadDeadline(time.Now().Add(pongWait))
    client.conn.SetPongHandler(func(string) error { 
        client.conn.SetReadDeadline(time.Now().Add(pongWait))
        return nil 
    })

    // 2. Setup Ticker untuk Kirim Ping (Goroutine terpisah)
    go func() {
		ticker := time.NewTicker(pingPeriod)
		defer ticker.Stop()

		// PERBAIKAN: Gunakan 'for range' alih-alih 'for { select { case ... } }'
		for range ticker.C {
			client.mu.Lock()
			if err := client.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				client.mu.Unlock()
				return // Putus koneksi jika gagal ping (koneksi ditutup atau error jaringan)
			}
			client.mu.Unlock()
		}
	}()
	// 4. Atur cleanup (menggantikan ws.on('close'))
	defer func() {
		log.Println("[CLOSE] Klien terputus.")
		h.channelsMu.Lock() // Kunci penuh untuk modifikasi
		defer h.channelsMu.Unlock()

		for channel, clients := range h.channels {
			if _, ok := clients[client]; ok {
				delete(clients, client)
				log.Printf("[UNSUB] Klien dihapus dari channel: %s. Sisa: %d", channel, len(clients))

				if len(clients) == 0 {
					delete(h.channels, channel)
					log.Printf("[STREAM] Klien terakhir untuk %s terputus. Reader akan berhenti.", channel)
				}
			}
		}
		client.conn.Close()
	}()

	// Read loop
	for {
		_, message, err := client.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Printf("[WSS] WebSocket error: %v", err)
			}
			break // Keluar dari loop untuk memicu defer cleanup
		}

		var msg IncomingMessage
		if err := json.Unmarshal(message, &msg); err != nil {
			log.Printf("Menerima format pesan non-JSON: %s", message)
			continue
		}

		h.metrics.messagesIn.Add(1)

		switch msg.Action {
		case "subscribe":
			if msg.Channel == "" {
				continue
			}
			h.handleSubscribe(client, &msg)
		case "unsubscribe":
			if msg.Channel == "" {
				continue
			}
			h.handleUnsubscribe(client, &msg)

		case "publish":
			if msg.Channel == "" || len(msg.Data) == 0 || msg.MessageID == "" {
				client.SendJSON(map[string]string{"error": "Format publish salah. Wajib ada: channel, data, messageId"})
				continue
			}
			h.handlePublish(client, &msg)

		default:
			log.Printf("Menerima aksi tidak dikenal: %s", msg.Action)
			client.SendJSON(map[string]string{"error": "Aksi tidak dikenal: " + msg.Action})
		}
	}
}

// handleSubscribe (logika dari 'case subscribe')
func (h *Hub) handleSubscribe(client *Client, msg *IncomingMessage) {
	h.channelsMu.Lock() // Kunci penuh untuk modifikasi
	if _, ok := h.channels[msg.Channel]; !ok {
		h.channels[msg.Channel] = make(map[*Client]bool)
	}
	h.channels[msg.Channel][client] = true
	log.Printf("[SUB] Klien subscribe ke channel: %s. Total: %d", msg.Channel, len(h.channels[msg.Channel]))
	h.channelsMu.Unlock()

	ctx := context.Background()

	// 1. Dapatkan ID pesan terakhir SAAT INI (batas atas untuk catch-up)
	// XRevRange mengambil 1 pesan terakhir. "+" adalah ID tertinggi, "-" adalah ID terendah.
	streams, err := h.redisClient.XRevRangeN(ctx, msg.Channel, "+", "-", 1).Result()

	// Inisialisasi endID untuk riwayat. Jika stream kosong, nilainya akan menjadi "+"
	var endID string = "+"

	if err != nil && err != redis.Nil {
		log.Printf("[CATCH-UP] Gagal mendapatkan ID terakhir stream %s: %v", msg.Channel, err)
	} else if len(streams) > 0 {
		// Jika ada pesan, gunakan ID pesan terakhir sebagai batas (End ID)
		endID = streams[0].ID
		log.Printf("[CATCH-UP] Batas Atas (End ID) untuk riwayat %s: %s", msg.Channel, endID)
	}

	// Tentukan ID awal untuk catch-up (Start ID)
	startID := "0" // Mulai dari awal stream
	if msg.LastMessageID != "" && msg.LastMessageID != "$" {
		startID = msg.LastMessageID
	}

	// Mulai goroutine untuk listener real-time (jika belum ada)
	// Kita passing EndID. Reader real-time akan mulai dari ID ini (tepat setelah riwayat)
	go h.ensureRealtimeReader(msg.Channel, endID)

	// Mulai goroutine untuk mengirim riwayat pesan ke klien INI SAJA
	// Kita passing StartID dan EndID. Riwayat akan dikirim dalam rentang [startID, endID]
	go h.sendHistoricalMessages(client, msg.Channel, startID, endID)

	client.SendJSON(map[string]string{"status": "subscribed", "channel": msg.Channel})
}

// handleUnsubscribe menghapus klien dari channel tertentu
func (h *Hub) handleUnsubscribe(client *Client, msg *IncomingMessage) {
	h.channelsMu.Lock()
	defer h.channelsMu.Unlock()

	if clients, ok := h.channels[msg.Channel]; ok {
		delete(clients, client)
		log.Printf("[UNSUB] Klien unsubscribe manual dari channel: %s. Sisa: %d", msg.Channel, len(clients))

		// Jika channel kosong, hapus channel dari map agar hemat memori
		if len(clients) == 0 {
			delete(h.channels, msg.Channel)
            // Catatan: Goroutine reader akan berhenti sendiri nanti saat mendeteksi subscriberCount == 0
		}
	}

	client.SendJSON(map[string]string{
		"status":  "unsubscribed",
		"channel": msg.Channel,
	})
}

// handlePublish (logika dari 'case publish')
func (h *Hub) handlePublish(client *Client, msg *IncomingMessage) {
	if !h.isRedisReady.Load() {
		client.SendJSON(map[string]string{
			"status":    "error_ack",
			"messageId": msg.MessageID,
			"error":     "Server Redis tidak siap",
		})
		return
	}

	log.Printf("[PUB] Klien mem-publish ke channel: %s", msg.Channel)

	startTime := time.Now()
	// Gunakan json.RawMessage (msg.Data) secara langsung
	streamID, err := h.redisClient.XAdd(context.Background(), &redis.XAddArgs{
		Stream: msg.Channel,
		Values: map[string]interface{}{"messageData": string(msg.Data)},
	}).Result()

	if err != nil {
		log.Printf("❌ [REDIS-STREAM] GAGAL XADD ke Redis: %v", err)
		client.SendJSON(map[string]string{
			"status":    "error_ack",
			"messageId": msg.MessageID,
			"error":     err.Error(),
		})
		return
	}

	// =========================================================================
	// !!! PERBAIKAN DIMULAI DI SINI: Atur TTL Key Channel 24 Jam !!!
	// =========================================================================
	// Set TTL 24 jam. Jika key sudah ada, TTL akan diperbarui (Expire).
	// Ini memastikan channel hanya dihapus jika tidak ada pesan (aktivitas) selama 24 jam.
	const streamTTL = 24 * time.Hour
	if err := h.redisClient.Expire(context.Background(), msg.Channel, streamTTL).Err(); err != nil {
		log.Printf("❌ [REDIS-STREAM] GAGAL mengatur TTL %s untuk %s: %v", streamTTL.String(), msg.Channel, err)
	} else {
		log.Printf("✅ [REDIS-STREAM] TTL %s diatur/diperbarui untuk %s", streamTTL.String(), msg.Channel)
	}
	// =========================================================================
	// !!! PERBAIKAN SELESAI !!!
	// =========================================================================

	latency := time.Since(startTime)
	h.metrics.publishLatencyTotal.Add(uint64(latency.Nanoseconds()))
	h.metrics.publishCount.Add(1)

	client.SendJSON(map[string]string{"status": "ack", "messageId": msg.MessageID})

	// Gunakan variabel streamID di dalam log
	log.Printf("[REDIS-STREAM] Berhasil XADD ke %s (ID: %s, Latency: %v)", msg.Channel, streamID, latency)
}

// sendHistoricalMessages (Porting dari fungsi JS)
// Mengirim pesan riwayat ke SATU klien spesifik dalam rentang ID [startID, endID]
func (h *Hub) sendHistoricalMessages(client *Client, channel string, startID string, endID string) {
	// Jika endID adalah default ("+"), berarti stream kosong saat subscribe, tidak perlu catch-up.
	if endID == "+" {
		log.Printf("[CATCH-UP] Stream %s kosong saat subscribe. Tidak ada riwayat untuk diambil.", channel)
		return
	}

	log.Printf("[CATCH-UP] Mengambil riwayat untuk %s dari ID: %s hingga %s...", channel, startID, endID)
	ctx := context.Background()

	// Ganti XRead loop tak terbatas dengan XRange yang dibatasi oleh startID dan endID.
	messages, err := h.redisClient.XRange(ctx, channel, startID, endID).Result()

	if err == redis.Nil || len(messages) == 0 {
		log.Printf("[CATCH-UP] Tidak ada pesan riwayat yang ditemukan dalam rentang %s-%s.", startID, endID)
		return
	}
	if err != nil {
		log.Printf("[CATCH-UP] Error mengambil riwayat %s: %v", channel, err)
		return
	}

	for _, msg := range messages {

		var data interface{}
		// Ambil data pesan dari map
		messageDataStr, ok := msg.Values["messageData"].(string)
		if !ok {
			log.Println("[CATCH-UP] Gagal mengambil messageData dari stream")
			continue
		}
		// Unmarshal string JSON ke interface{}
		if err := json.Unmarshal([]byte(messageDataStr), &data); err != nil {
			log.Printf("[CATCH-UP] Gagal parse JSON dari stream: %v", err)
			continue
		}

		broadcastMessage := OutgoingMessage{
			Event:    "message",
			Channel:  channel,
			StreamID: msg.ID,
			Data:     data,
		}

		if err := client.SendJSON(broadcastMessage); err != nil {
			// Klien mungkin terputus saat catch-up
			log.Printf("[CATCH-UP] Gagal mengirim pesan riwayat ke klien, berhenti: %v", err)
			return // Hentikan goroutine ini
		}
		h.metrics.messagesOut.Add(1)
	}
	log.Printf("[CATCH-UP] Selesai mengambil riwayat untuk %s. Total pesan: %d.", channel, len(messages))
}

// ensureRealtimeReader (Porting dari fungsi JS)
// Memastikan HANYA SATU listener real-time per channel
// Memulai XREAD BLOCK tepat setelah pesan riwayat terakhir (lastSeenID)
func (h *Hub) ensureRealtimeReader(channel string, lastSeenID string) {
	// Cek 1: Apakah sudah ada reader yang berjalan? (Aman-konkurensi)
	h.readerMu.Lock()
	if h.activeStreamReaders[channel] {
		h.readerMu.Unlock()
		return // Sudah ada, tidak perlu melakukan apa-apa
	}
	// Tandai sebagai aktif SEBELUM membuka kunci
	h.activeStreamReaders[channel] = true
	h.readerMu.Unlock()

	// Tentukan ID awal untuk reader real-time.
	// Jika lastSeenID adalah "+", berarti stream kosong saat subscribe, mulai dari ID terbaru "$".
	// Jika ada ID (misal: "1664162000000-99"), kita gunakan ID itu.
	currentID := lastSeenID
	if currentID == "+" {
		currentID = "$" // Mulai dari pesan terbaru
	}

	log.Printf("[STREAM] Memulai reader REAL-TIME untuk channel: %s dari ID: %s", channel, currentID)

	// Set cleanup: pastikan kita menghapus tanda 'aktif' saat reader berhenti
	defer func() {
		h.readerMu.Lock()
		delete(h.activeStreamReaders, channel)
		h.readerMu.Unlock()
		log.Printf("[STREAM] Menghentikan reader REAL-TIME untuk channel: %s", channel)
	}()

	ctx := context.Background()

	for {
		// Cek 2: Apakah masih ada subscriber? (Aman-konkurensi)
		h.channelsMu.RLock() // Kunci baca
		clientSet, ok := h.channels[channel]
		subscriberCount := len(clientSet)
		h.channelsMu.RUnlock() // Lepas kunci baca

		if !ok || subscriberCount == 0 {
			break // Tidak ada subscriber, hentikan loop
		}

		if !h.isRedisReady.Load() {
			log.Printf("[STREAM] Redis tidak siap, reader %s dijeda...", channel)
			time.Sleep(2 * time.Second)
			continue
		}

		// XRead blocking
		// currentID sudah diinisialisasi dengan ID yang tepat (setelah riwayat atau "$")
		response, err := h.subscriberClient.XRead(ctx, &redis.XReadArgs{
			Streams: []string{channel, currentID},
			Count:   100,
			Block:   5 * time.Second, // Blok selama 5 detik
		}).Result()

		if err == redis.Nil {
			continue // Timeout, loop lagi
		}
		if err != nil {
			log.Printf("[STREAM] Error di XREAD untuk %s: %v", channel, err)
			time.Sleep(2 * time.Second) // Tunggu sebelum mencoba lagi
			continue
		}

		if len(response) == 0 {
			continue
		}

		// Ambil daftar klien saat ini TEPAT SEBELUM broadcast
		h.channelsMu.RLock()
		clientsToBroadcast := make([]*Client, 0, len(h.channels[channel]))
		if clientSet, ok := h.channels[channel]; ok {
			for client := range clientSet {
				clientsToBroadcast = append(clientsToBroadcast, client)
			}
		}
		h.channelsMu.RUnlock()

		if len(clientsToBroadcast) == 0 {
			continue // Klien mungkin disconnect saat kita XREAD
		}

		messages := response[0].Messages
		for _, msg := range messages {
			currentID = msg.ID // Perbarui ID untuk iterasi XREAD berikutnya

			var data interface{}
			messageDataStr, ok := msg.Values["messageData"].(string)
			if !ok {
				log.Println("[STREAM] Gagal mengambil messageData dari stream")
				continue
			}
			if err := json.Unmarshal([]byte(messageDataStr), &data); err != nil {
				log.Printf("[STREAM] Gagal parse JSON dari stream: %v", err)
				continue
			}

			broadcastMessage := OutgoingMessage{
				Event:    "message",
				Channel:  channel,
				StreamID: msg.ID,
				Data:     data,
			}

			// Broadcast ke semua klien yang terdaftar
			for _, client := range clientsToBroadcast {
				// Kita bisa menjalankan ini di goroutine baru jika pengiriman lambat
				// tapi untuk kesederhanaan, kita lakukan secara sekuensial
				if err := client.SendJSON(broadcastMessage); err != nil {
					log.Printf("[STREAM] Gagal kirim ke klien: %v", err)
					// Klien akan dihapus oleh read-loopnya sendiri
				}
				h.metrics.messagesOut.Add(1)
			}
		}
	}
}

// --- Fungsi Main ---
func main() {
	// Muat .env file
	if err := godotenv.Load(); err != nil {
		log.Println("Peringatan: Tidak dapat memuat file .env. Mengandalkan variabel lingkungan.")
	}

	// Baca dan validasi Env Vars
	websocketSecretKey = os.Getenv("WEBSOCKET_SECRET_KEY")
	redisURL = os.Getenv("REDIS_URL")
	redisPubPoolSize = os.Getenv("REDIS_PUB_POOL_SIZE")
	redisSubPoolSize = os.Getenv("REDIS_SUB_POOL_SIZE")
	port = os.Getenv("PORT")

	if websocketSecretKey == "" {
		log.Fatal("ERROR: WEBSOCKET_SECRET_KEY tidak diatur di file .env")
	}
	if redisURL == "" {
		// Gunakan default dari docker-compose.yaml jika ada
		redisURL = "redis://redis:6379"
		log.Printf("Peringatan: REDIS_URL tidak diatur. Menggunakan default: %s", redisURL)
	}
	if port == "" {
		port = "8080"
		log.Printf("Peringatan: PORT tidak diatur. Menggunakan default: %s", port)
	}

	if redisPubPoolSize == "" {
		redisPubPoolSize = "50"
		log.Printf("Peringatan: REDIS_PUB_POOL_SIZE tidak diatur. Menggunakan default: %s", redisPubPoolSize)
	}

	if redisSubPoolSize == "" {
		redisSubPoolSize = "50"
		log.Printf("Peringatan: REDIS_SUB_POOL_SIZE tidak diatur. Menggunakan default: %s", redisSubPoolSize)
	}
	// Buat Hub
	hub := NewHub()

	// Daftarkan handler
	http.HandleFunc("/", hub.handleWebSocket) // Asumsi endpoint utama

	// Mulai server
	log.Printf("🚀 Server WebSocket (versi Go) berjalan di ws://localhost:%s", port)
	if err := http.ListenAndServe(":"+port, nil); err != nil {
		log.Fatalf("Gagal memulai server: %v", err)
	}
}
