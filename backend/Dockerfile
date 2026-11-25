FROM golang:1.26-alpine

WORKDIR /app

COPY go.mod ./
RUN go mod download

COPY . .

EXPOSE 8080

CMD ["sh", "-c", "go build -o /tmp/wbd-backend . && exec /tmp/wbd-backend"]