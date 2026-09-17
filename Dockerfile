FROM golang:1.27-alpine as builder

WORKDIR /project-surge

COPY go.mod go.sum ./

RUN go mod download

COPY . .

RUN go build -o main .


FROM alpine:3.23.5

WORKDIR /project-surge

COPY --from=builder /project-surge/main .

EXPOSE 8080

CMD ["./main"]


