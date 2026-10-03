# Stage 1: compile. Go lives here, not on the host.
FROM golang:1.23-alpine AS build
WORKDIR /src

# copied first so `go mod download` is cached until deps actually change
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /out/api ./cmd/api

# Stage 2: run. No shell, no package manager, no Go toolchain. ~12MB.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/api /api
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/api"]
