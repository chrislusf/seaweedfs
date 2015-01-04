package weed_server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/chrislusf/weed-fs/go/glog"
	"github.com/chrislusf/weed-fs/go/operation"
	"github.com/chrislusf/weed-fs/go/stats"
	"github.com/chrislusf/weed-fs/go/storage"
	"github.com/chrislusf/weed-fs/go/util"
)

var serverStats *stats.ServerStats

func init() {
	serverStats = stats.NewServerStats()
	go serverStats.Start()

}

func writeJson(w http.ResponseWriter, r *http.Request, obj interface{}) (err error) {
	var b []byte
	if r.FormValue("pretty") != "" {
		b, err = json.MarshalIndent(obj, "", "  ")
		// to show & for human
		if err == nil {
			b = bytes.Replace(b, []byte("\\u0026"), []byte("&"), -1)
		}
	} else {
		b, err = json.Marshal(obj)
	}
	if err != nil {
		return
	}
	callback := r.FormValue("callback")
	if callback == "" {
		w.Header().Set("Content-Type", "application/json")
		_, err = w.Write(b)
	} else {
		w.Header().Set("Content-Type", "application/javascript")
		if _, err = w.Write([]uint8(callback)); err != nil {
			return
		}
		if _, err = w.Write([]uint8("(")); err != nil {
			return
		}
		fmt.Fprint(w, string(b))
		if _, err = w.Write([]uint8(")")); err != nil {
			return
		}
	}
	return
}

// wrapper for writeJson - just logs errors
func writeJsonQuiet(w http.ResponseWriter, r *http.Request, obj interface{}) {
	if err := writeJson(w, r, obj); err != nil {
		glog.V(0).Infof("error writing JSON %s: %s", obj, err.Error())
	}
}
func writeJsonError(w http.ResponseWriter, r *http.Request, err error) {
	w.WriteHeader(http.StatusInternalServerError)
	m := make(map[string]interface{})
	m["error"] = err.Error()
	writeJsonQuiet(w, r, m)
}

func debug(params ...interface{}) {
	glog.V(4).Infoln(params)
}

func secure(whiteList []string, f func(w http.ResponseWriter, r *http.Request)) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		if len(whiteList) == 0 {
			f(w, r)
			return
		}
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err == nil {
			for _, ip := range whiteList {
				if ip == host {
					f(w, r)
					return
				}
			}
		}
		writeJsonQuiet(w, r, map[string]interface{}{"error": "No write permisson from " + host})
	}
}

func submitForClientHandler(w http.ResponseWriter, r *http.Request, masterUrl string) {
	m := make(map[string]interface{})
	if r.Method != "POST" {
		m["error"] = "Only submit via POST!"
		writeJsonQuiet(w, r, m)
		return
	}

	debug("parsing upload file...")
	fname, data, mimeType, isGzipped, lastModified, _, pe := storage.ParseUpload(r)
	if pe != nil {
		writeJsonError(w, r, pe)
		return
	}

	debug("assigning file id for", fname)
	assignResult, ae := operation.Assign(masterUrl, 1, r.FormValue("replication"), r.FormValue("collection"), r.FormValue("ttl"))
	if ae != nil {
		writeJsonError(w, r, ae)
		return
	}

	url := "http://" + assignResult.PublicUrl + "/" + assignResult.Fid
	if lastModified != 0 {
		url = url + "?ts=" + strconv.FormatUint(lastModified, 10)
	}

	debug("upload file to store", url)
	uploadResult, err := operation.Upload(url, fname, bytes.NewReader(data), isGzipped, mimeType)
	if err != nil {
		writeJsonError(w, r, err)
		return
	}

	m["fileName"] = fname
	m["fid"] = assignResult.Fid
	m["fileUrl"] = assignResult.PublicUrl + "/" + assignResult.Fid
	m["size"] = uploadResult.Size
	writeJsonQuiet(w, r, m)
	return
}

func deleteForClientHandler(w http.ResponseWriter, r *http.Request, masterUrl string) {
	r.ParseForm()
	fids := r.Form["fid"]
	ret, err := operation.DeleteFiles(masterUrl, fids)
	if err != nil {
		writeJsonError(w, r, err)
		return
	}
	writeJsonQuiet(w, r, ret)
}

func parseURLPath(path string) (vid, fid, filename, ext string, isVolumeIdOnly bool) {
	switch strings.Count(path, "/") {
	case 3:
		parts := strings.Split(path, "/")
		vid, fid, filename = parts[1], parts[2], parts[3]
		ext = filepath.Ext(filename)
	case 2:
		parts := strings.Split(path, "/")
		vid, fid = parts[1], parts[2]
		dotIndex := strings.LastIndex(fid, ".")
		if dotIndex > 0 {
			ext = fid[dotIndex:]
			fid = fid[0:dotIndex]
		}
	default:
		sepIndex := strings.LastIndex(path, "/")
		commaIndex := strings.LastIndex(path[sepIndex:], ",")
		if commaIndex <= 0 {
			vid, isVolumeIdOnly = path[sepIndex+1:], true
			return
		}
		dotIndex := strings.LastIndex(path[sepIndex:], ".")
		vid = path[sepIndex+1 : commaIndex]
		fid = path[commaIndex+1:]
		ext = ""
		if dotIndex > 0 {
			fid = path[commaIndex+1 : dotIndex]
			ext = path[dotIndex:]
		}
	}
	return
}

func statsCounterHandler(w http.ResponseWriter, r *http.Request) {
	m := make(map[string]interface{})
	m["Version"] = util.VERSION
	m["Counters"] = serverStats
	writeJsonQuiet(w, r, m)
}

func statsMemoryHandler(w http.ResponseWriter, r *http.Request) {
	m := make(map[string]interface{})
	m["Version"] = util.VERSION
	m["Memory"] = stats.MemStat()
	writeJsonQuiet(w, r, m)
}

func helpHandler(w http.ResponseWriter, r *http.Request) {
	m := make(map[string]interface{})
	m["Version"] = util.VERSION
	examples := make([]string, 0, 16)
	examples = append(examples, "/dir/assign")
	examples = append(examples, "/dir/lookup")
	examples = append(examples, "/dir/lookup?volumeId=3,01637037d6")
	examples = append(examples, "/dir/status")
	examples = append(examples, "/col/delete?collection=benchmark&pretty=y")
	examples = append(examples, "/vol/grow?dataCenter=dc1&count=4")
	examples = append(examples, "/vol/grow?collection=turbo&count=4")
	examples = append(examples, "/vol/grow?dataCenter=dc1&count=4")
	examples = append(examples, "/vol/status")
	examples = append(examples, "/stats/counter")
	examples = append(examples, "/stats/memory")
	m["Examples"] = examples

	r.ParseForm()
	r.Form["pretty"] = make([]string, 0)
	r.Form["pretty"] = append(r.Form["pretty"], "y")
	writeJsonQuiet(w, r, m)
}
