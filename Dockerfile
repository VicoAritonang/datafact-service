# Stage 1: Build
FROM golang:1.23-alpine AS builder

WORKDIR /app

# Copy go mod dan download dependencies
COPY go.mod ./
# COPY go.sum ./ # (Uncomment baris ini nanti jika sudah ada go.sum setelah go mod tidy)
RUN go mod download

# Copy source code
COPY . .

# Build aplikasi menjadi binary bernama "server"
RUN go build -o server main.go

# Stage 2: Run (Image kecil agar hemat storage & cepat)
FROM alpine:latest
WORKDIR /root/
COPY --from=builder /app/server .

# Install sertifikat SSL (penting untuk HTTPS request ke Google/Supabase)
RUN apk --no-cache add ca-certificates

# Expose port 8080
EXPOSE 8080

# Jalankan server
CMD ["./server"]