FROM golang:1.11 AS builder
ADD https://github.com/golang/dep/releases/download/v0.5.3/dep-linux-amd64  /usr/bin/dep
RUN chmod +x /usr/bin/dep
ENV APP_DIR=$GOPATH/src/rpc/
RUN mkdir -p $APP_DIR
COPY ./* $APP_DIR
WORKDIR $APP_DIR
RUN mkdir -p /opt/rpc

###
# Copy the code from the host and compile it
#WORKDIR $GOPATH/src/github.com/username/repo
COPY Gopkg.toml Gopkg.lock ./
RUN dep ensure --vendor-only
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix nocgo -o /opt/rpc/rpc *.go
COPY .env config.json /opt/rpc/

#FROM scratch
FROM alpine:3.9 as prod_img
RUN mkdir -p /opt/rpc/
RUN mkdir -p /usr/share/asterisk/sounds/ru/t2s/
RUN apk update && apk add ca-certificates && apk add tzdata && apk add sox && apk add lame
COPY --from=builder /opt/rpc/* /opt/rpc/
RUN cp /usr/share/zoneinfo/Europe/Moscow /etc/localtime
RUN echo "Europe/Moscow" >  /etc/timezone
#TZ=Europe/Moscow
#cp /usr/share/zoneinfo/$TZ /etc/localtime && echo $TZ > /etc/timezone
WORKDIR /opt/rpc/
ENTRYPOINT ["/opt/rpc/rpc"]

