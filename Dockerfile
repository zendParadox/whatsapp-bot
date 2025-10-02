# Bagian 1: Build Stage
# Menggunakan image resmi Go sebagai basis untuk meng-compile aplikasi
FROM golang:1.21-alpine as builder

# Menetapkan direktori kerja di dalam container
WORKDIR /app

# Menyalin file go.mod dan go.sum untuk mengunduh dependensi
COPY go.mod go.sum ./
RUN go mod download

# Menyalin seluruh sisa kode sumber
COPY . .

# Meng-compile aplikasi Go.
# CGO_ENABLED=0 penting untuk membuat binary statis yang tidak bergantung pada library C
# -o /bin/app berarti output hasil compile akan disimpan sebagai file 'app' di folder /bin
RUN CGO_ENABLED=0 go build -o /bin/app

# Bagian 2: Final Stage
# Menggunakan image Alpine Linux yang sangat kecil sebagai basis akhir
FROM alpine:latest

# Menyalin binary 'app' yang sudah di-compile dari stage 'builder'
COPY --from=builder /bin/app /bin/app

# Sesi login akan dibuat dan disimpan di Persistent Disk saat pertama kali dijalankan,
# sehingga kita tidak perlu menyalin file wa-session.db ke dalam image.

# Menetapkan direktori kerja
WORKDIR /app

# Perintah yang akan dijalankan saat container dimulai
CMD ["/bin/app"]
