FROM cydev/go
RUN go get github.com/chrislusf/weed-fs/go/weed
EXPOSE 8080
EXPOSE 9333
VOLUME /data
ENTRYPOINT ["weed"]
