# syntax=docker/dockerfile:1

FROM golang:1.22 AS build

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . ./

RUN go build -o /dtd2mysql-runner .

FROM gcr.io/distroless/base-debian12 AS release

WORKDIR /

COPY --from=build /dtd2mysql-runner /dtd2mysql-runner

USER nonroot:nonroot

ENTRYPOINT ["/dtd2mysql-runner"]
