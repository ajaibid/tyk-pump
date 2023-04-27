FROM golang:1.18 as build

WORKDIR /go/src/app
COPY . .
RUN ["mkdir", "-p", "/opt/tyk-pump/tyk-pump"]
RUN CGO_ENABLED=0 go build -o /opt/tyk-pump/tyk-pump

FROM gcr.io/distroless/static-debian11

COPY --from=build /opt/tyk-pump /opt/
WORKDIR /opt/tyk-pump
CMD ["/opt/tyk-pump/tyk-pump"]
