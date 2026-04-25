FROM golang:1.26.2 AS builder

WORKDIR /workspace

COPY go.mod go.sum ./
RUN go mod download

COPY *.go .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o alertmanager-digest .


FROM scratch

COPY --from=builder /workspace/alertmanager-digest /

ENTRYPOINT ["/alertmanager-digest"]
