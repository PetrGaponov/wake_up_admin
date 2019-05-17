FROM golang:1.11 AS builder

# Download and install the latest release of dep
ADD https://github.com/golang/dep/releases/download/v0.5.3/dep-linux-amd64  /usr/bin/dep
RUN chmod +x /usr/bin/dep
##
ENV APP_DIR=$GOPATH/src/rest/
RUN mkdir -p $APP_DIR
COPY ./* $APP_DIR
WORKDIR $APP_DIR
RUN mkdir -p /opt/rest

###
# Copy the code from the host and compile it
#WORKDIR $GOPATH/src/github.com/username/repo
COPY Gopkg.toml Gopkg.lock ./
RUN dep ensure --vendor-only
#dep ensure --vendor-only
#COPY . ./
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix nocgo -o /opt/rest/rest *.go
COPY .env config.json /opt/rest/
#RUN  go  build -o /opt/rest/rest ./*.go

#FROM scratch
FROM alpine:3.9 as prod_img
RUN mkdir -p /opt/rest
COPY --from=builder /opt/rest/* /opt/rest/
WORKDIR /opt/rest/
EXPOSE 8080
ENTRYPOINT ["/opt/rest/rest"]