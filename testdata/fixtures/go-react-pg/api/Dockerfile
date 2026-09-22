FROM golang:1-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -o /out/api .

FROM alpine:3.20
WORKDIR /app
COPY --from=build /out/api /usr/local/bin/api
COPY migrations ./migrations
COPY seed ./seed
EXPOSE 8080
CMD ["api", "serve"]
