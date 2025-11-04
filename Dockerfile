# --- Tahap 1: Builder ---
# Gunakan tag 'alpine' yang benar untuk Go terbaru di Alpine
FROM golang:alpine AS builder

WORKDIR /app

# Salin file modul terlebih dahulu untuk cache dependensi
COPY go.mod go.sum ./
RUN go mod download

# Salin sisa kode sumber
COPY . .

# Build binary
# CGO_ENABLED=0 penting untuk static linking di Alpine
# -o /app/server akan membuat file executable bernama 'server'
RUN CGO_ENABLED=0 go build -o /app/server .

# --- Tahap 2: Produksi ---
# Gunakan image Alpine yang ramping
FROM alpine:latest

WORKDIR /app

# Salin HANYA binary yang sudah di-build dari tahap builder
COPY --from=builder /app/server .

# Ekspos port yang akan digunakan
EXPOSE 8080

# Perintah untuk menjalankan aplikasi
# Ini akan menjalankan file executable 'server'
CMD [ "./server" ]