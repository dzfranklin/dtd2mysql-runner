# syntax=docker/dockerfile:1

FROM golang:1.22 AS build

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . ./

RUN go build -o /dtd2mysql-runner .

FROM ubuntu:20.04 AS release

WORKDIR /app

RUN apt update && apt install -y --no-install-recommends wget default-jre && \
    rm -rf /var/lib/apt/lists/*

COPY --from=build /dtd2mysql-runner /dtd2mysql-runner

RUN wget "https://github.com/MobilityData/gtfs-validator/releases/download/v5.0.1/gtfs-validator-5.0.1-cli.jar" -O "gtfs-validator.jar"

ENTRYPOINT ["/app/dtd2mysql-runner"]
