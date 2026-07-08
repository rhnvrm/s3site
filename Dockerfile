FROM golang:1.26.2-alpine3.23 AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
ARG COMMIT=none
ARG DATE=unknown
ARG TARGETARCH=amd64

RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build \
  -trimpath \
  -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.date=${DATE}" \
  -o /out/s3site ./cmd/s3site

FROM gcr.io/distroless/static-debian12
COPY --from=build /out/s3site /s3site
EXPOSE 80
ENTRYPOINT ["/s3site"]
CMD ["-listen", ":80"]
