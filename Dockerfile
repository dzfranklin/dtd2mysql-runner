# syntax=docker/dockerfile:1

FROM golang:1.22 AS build

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . ./

RUN go build -o /dtd2mysql-runner .

FROM ubuntu:20.04 AS release

WORKDIR /app

COPY --from=build /dtd2mysql-runner /dtd2mysql-runner

ENTRYPOINT ["/dtd2mysql-runner"]
