package command

import (
	"net/http"
	"time"

	"fmt"
	"github.com/chrislusf/seaweedfs/weed/glog"
	"github.com/chrislusf/seaweedfs/weed/s3api"
	"github.com/chrislusf/seaweedfs/weed/util"
	"github.com/gorilla/mux"
)

var (
	s3options S3Options
)

type S3Options struct {
	filer            *string
	filerGrpcPort    *int
	filerBucketsPath *string
	port             *int
	domainName       *string
	tlsPrivateKey    *string
	tlsCertificate   *string
}

func init() {
	cmdS3.Run = runS3 // break init cycle
	s3options.filer = cmdS3.Flag.String("filer", "localhost:8888", "filer server address")
	s3options.filerGrpcPort = cmdS3.Flag.Int("filer.grpcPort", 0, "filer server grpc port, default to filer http port plus 10000")
	s3options.filerBucketsPath = cmdS3.Flag.String("filer.dir.buckets", "/buckets", "folder on filer to store all buckets")
	s3options.port = cmdS3.Flag.Int("port", 8333, "s3options server http listen port")
	s3options.domainName = cmdS3.Flag.String("domainName", "", "suffix of the host name, {bucket}.{domainName}")
	s3options.tlsPrivateKey = cmdS3.Flag.String("key.file", "", "path to the TLS private key file")
	s3options.tlsCertificate = cmdS3.Flag.String("cert.file", "", "path to the TLS certificate file")
}

var cmdS3 = &Command{
	UsageLine: "s3 -port=8333 -filer=<ip:port>",
	Short:     "start a s3 API compatible server that is backed by a filer",
	Long: `start a s3 API compatible server that is backed by a filer.

`,
}

func runS3(cmd *Command, args []string) bool {
	filerGrpcAddress, err := parseFilerGrpcAddress(*s3options.filer, *s3options.filerGrpcPort)
	util.LogFatalIfError(err, "S3 API Server Fail to parseFilerGrpcAddress: %v", err)

	router := mux.NewRouter().SkipClean(true)

	_, err = s3api.NewS3ApiServer(router, &s3api.S3ApiServerOption{
		Filer:            *s3options.filer,
		FilerGrpcAddress: filerGrpcAddress,
		DomainName:       *s3options.domainName,
		BucketsPath:      *s3options.filerBucketsPath,
	})
	util.LogFatalIfError(err, "S3 API Server startup error: %v", err)

	httpS := &http.Server{Handler: router}

	listenAddress := fmt.Sprintf(":%d", *s3options.port)
	s3ApiListener, err := util.NewListener(listenAddress, time.Duration(10)*time.Second)
	util.LogFatalIfError(err, "S3 API Server listener on %s error: %v", listenAddress, err)

	if *s3options.tlsPrivateKey != "" {
		err = httpS.ServeTLS(s3ApiListener, *s3options.tlsCertificate, *s3options.tlsPrivateKey)
		util.LogFatalIfError(err, "S3 API Server Fail to serve: %v", err)

		glog.V(0).Infof("Start Seaweed S3 API Server %s at https port %d", util.VERSION, *s3options.port)
	} else {
		err = httpS.Serve(s3ApiListener)
		util.LogFatalIfError(err, "S3 API Server Fail to serve: %v", err)

		glog.V(0).Infof("Start Seaweed S3 API Server %s at http port %d", util.VERSION, *s3options.port)
	}

	return true

}
