# Server WebSocket Go (dengan Redis Streams)

Ini adalah server WebSocket berkinerja tinggi yang ditulis dalam Go, dirancang untuk skalabilitas dan pesan *real-time* menggunakan Redis Streams.

Proyek ini adalah porting dari arsitektur Node.js dan memiliki fitur:
* Otentikasi berbasis token.
* Manajemen *channel* (Subscribe/Publish).
* *Catch-up* riwayat pesan otomatis untuk klien yang baru terhubung.
* *Real-time reader* yang efisien menggunakan `XREAD BLOCK`.

## 1. ⚙️ Prasyarat

Sebelum Anda mulai, pastikan Anda telah menginstal:
* **Go**: Versi 1.25.3 atau lebih baru.
* **Redis**: Server Redis yang sedang berjalan (baik lokal, di server, atau di Docker).
* **Docker**: Diperlukan jika Anda ingin menjalankan menggunakan `docker-compose`.

## 2. 🔑 Konfigurasi

Aplikasi ini dikonfigurasi menggunakan variabel lingkungan.

1.  Salin file `.env.example` menjadi file baru bernama `.env`.
    ```bash
    cp .env.example .env
    ```
2.  Edit file `.env` dan isi nilainya.

    ```ini
    # Kunci rahasia untuk otentikasi koneksi WebSocket
    WEBSOCKET_SECRET_KEY=supersecretkey123
    
    # URL koneksi ke server Redis Anda
    # (Lihat di bawah untuk skenario yang berbeda)
    REDIS_URL=redis://localhost:6379
    
    # Port tempat server Go WebSocket akan berjalan
    PORT=8080
    ```

### Pengaturan `REDIS_URL`

Pilih salah satu berdasarkan pengaturan Anda:

* **Untuk Menjalankan Lokal (`go run`):**
    ```ini
    REDIS_URL=redis://localhost:6379
    ```
* **Untuk Docker (Menghubungkan ke `localhost` Host):**
    ```ini
    REDIS_URL=redis://host.docker.internal:6379
    ```
* **Untuk Docker (Menghubungkan ke Server Lain):**
    ```ini
    REDIS_URL=redis://IP_SERVER_REDIS_ANDA:6379
    ```
* **Untuk Docker (Menghubungkan ke Kontainer Redis Lain):**
    (Asumsi Anda telah menghubungkan keduanya ke jaringan kustom yang sama)
    ```ini
    REDIS_URL=redis://nama_kontainer_redis:6379
    ```

## 3. 🚀 Cara Menjalankan

Ada dua cara untuk menjalankan server ini:

### A. Lokal (Untuk Pengembangan)

1.  Pastikan server Redis Anda berjalan dan `REDIS_URL` di `.env` sudah benar.
2.  Instal dependensi Go:
    ```bash
    go mod tidy
    ```
3.  Jalankan server:
    ```bash
    go run main.go
    ```
4.  Server akan berjalan di `ws://localhost:8080`.

### B. Docker (Disarankan untuk Produksi)

Metode ini akan mem-build aplikasi Go Anda di dalam kontainer Docker dan menjalankannya.

1.  Pastikan Docker sedang berjalan.
2.  Pastikan `REDIS_URL` di `.env` sudah diatur dengan benar (misalnya, ke `host.docker.internal` atau IP server).
3.  Bangun (build) dan jalankan kontainer menggunakan Docker Compose:
    ```bash
    docker-compose up --build
    ```
4.  Server akan berjalan dan diekspos di `ws://localhost:8080`.

Untuk menghentikan server, tekan `Ctrl + C` di terminal, lalu jalankan:
```bash
docker-compose down