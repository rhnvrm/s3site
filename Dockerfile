FROM golang:1.26.2-alpine3.23 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /out/s3site ./cmd/s3site

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/s3site /s3site
EXPOSE 8080
ENTRYPOINT ["/s3site"]
