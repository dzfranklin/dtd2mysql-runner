# syntax=docker/dockerfile:1

FROM golang:1.22 AS build

WORKDIR /app

RUN apt install --yes --no-install-recommends ca-certificates && \
    rm -rf /var/lib/apt/lists/*

COPY go.mod go.sum ./
RUN go mod download

COPY . ./

RUN go build -o /dtd2mysql-runner .

ENTRYPOINT ["/dtd2mysql-runner"]
