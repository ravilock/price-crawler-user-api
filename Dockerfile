FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY . .
RUN CGO_ENABLED=0 go build -o /api ./cmd/user-api
FROM alpine:3.21
RUN apk add --no-cache ca-certificates
COPY --from=build /api /api
EXPOSE 8080
ENTRYPOINT ["/api"]
