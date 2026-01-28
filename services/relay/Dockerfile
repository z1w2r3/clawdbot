FROM golang:1.22-alpine AS build
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /relay .

FROM alpine:3.19
RUN apk add --no-cache ca-certificates
COPY --from=build /relay /relay
EXPOSE 9000
ENTRYPOINT ["/relay"]
