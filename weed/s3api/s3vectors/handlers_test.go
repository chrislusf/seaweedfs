package s3vectors

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/seaweedfs/seaweedfs/weed/pb/vector_pb"
	"google.golang.org/grpc"
)

func TestArnParts(t *testing.T) {
	var v vectorBucketARNs

	if err := arnParts(&v, "plain", ""); err != nil || v.bucket != "plain" {
		t.Fatalf("plain name: bucket=%q err=%v", v.bucket, err)
	}
	if err := arnParts(&v, "", "arn:aws:s3vectors:r:a:bucket/mybucket"); err != nil || v.bucket != "mybucket" || v.index != "" {
		t.Fatalf("bucket ARN: %+v err=%v", v, err)
	}
	v = vectorBucketARNs{}
	if err := arnParts(&v, "", "arn:aws:s3vectors:r:a:bucket/mybucket/index/myindex"); err != nil || v.bucket != "mybucket" || v.index != "myindex" {
		t.Fatalf("index ARN: %+v err=%v", v, err)
	}
	for _, bad := range []string{
		"arn:aws:s3vectors:r:a:bucket/",
		"arn:aws:s3vectors:r:a:bucket//index/i",
		"arn:aws:s3vectors:r:a:bucket",
	} {
		v = vectorBucketARNs{}
		if err := arnParts(&v, "", bad); err == nil {
			t.Fatalf("ARN %q should be rejected", bad)
		}
	}
}

func TestResolveIndexARNs(t *testing.T) {
	arns, err := resolveIndexARNs("", "", "", "arn:aws:s3vectors:r:a:bucket/b/index/i")
	if err != nil || arns.bucket != "b" || arns.index != "i" {
		t.Fatalf("indexArn only: %+v err=%v", arns, err)
	}
	arns, err = resolveIndexARNs("b", "", "i", "")
	if err != nil || arns.bucket != "b" || arns.index != "i" {
		t.Fatalf("names only: %+v err=%v", arns, err)
	}
	if _, err := resolveIndexARNs("", "not-an-arn", "i", ""); err == nil {
		t.Fatal("non-ARN vectorBucketArn should be rejected")
	}
	if _, err := resolveIndexARNs("", "", "", ""); err == nil {
		t.Fatal("missing identifiers should be rejected")
	}
	if _, err := resolveIndexARNs("b", "", "", ""); err == nil {
		t.Fatal("missing indexName should be rejected")
	}
}

func TestTranslateFilter(t *testing.T) {
	cases := []struct {
		name   string
		filter map[string]interface{}
		want   string
	}{
		{"bare string equality", map[string]interface{}{"k": "v"},
			`json_get_string(` + "`metadata`" + `, 'k') = 'v'`},
		{"bare number equality", map[string]interface{}{"k": float64(3)},
			`json_get_float(` + "`metadata`" + `, 'k') = 3`},
		{"bare bool equality", map[string]interface{}{"k": true},
			`json_get_bool(` + "`metadata`" + `, 'k') = true`},
		{"ne", map[string]interface{}{"k": map[string]interface{}{"$ne": "v"}},
			`json_get_string(` + "`metadata`" + `, 'k') <> 'v'`},
		{"exists true", map[string]interface{}{"k": map[string]interface{}{"$exists": true}},
			`json_exists(` + "`metadata`" + `, '$["k"]')`},
		{"exists false", map[string]interface{}{"k": map[string]interface{}{"$exists": false}},
			`NOT json_exists(` + "`metadata`" + `, '$["k"]')`},
		{"exists special key", map[string]interface{}{`a.b"c`: map[string]interface{}{"$exists": true}},
			`json_exists(` + "`metadata`" + `, '$["a.b\"c"]')`},
		{"range", map[string]interface{}{"k": map[string]interface{}{"$gt": float64(1)}},
			`json_get_float(` + "`metadata`" + `, 'k') > 1`},
		{"in strings", map[string]interface{}{"k": map[string]interface{}{"$in": []interface{}{"a", "b"}}},
			`(json_get_string(` + "`metadata`" + `, 'k') in ('a') OR json_get_string(` + "`metadata`" + `, 'k') in ('b'))`},
		{"nin numbers", map[string]interface{}{"k": map[string]interface{}{"$nin": []interface{}{float64(1)}}},
			`NOT (json_get_float(` + "`metadata`" + `, 'k') in (1))`},
		{"and", map[string]interface{}{"$and": []interface{}{
			map[string]interface{}{"a": "1"}, map[string]interface{}{"b": float64(2)},
		}},
			`(json_get_string(` + "`metadata`" + `, 'a') = '1' AND json_get_float(` + "`metadata`" + `, 'b') = 2)`},
		{"sql injection", map[string]interface{}{"a'b": "v'1"},
			`json_get_string(` + "`metadata`" + `, 'a''b') = 'v''1'`},
	}
	for _, tc := range cases {
		got, err := translateFilter(tc.filter)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got != tc.want {
			t.Fatalf("%s:\n got %s\nwant %s", tc.name, got, tc.want)
		}
	}

	for name, filter := range map[string]map[string]interface{}{
		"unknown operator":  {"k": map[string]interface{}{"$bogus": "v"}},
		"exists non-bool":   {"k": map[string]interface{}{"$exists": "yes"}},
		"range non-number":  {"k": map[string]interface{}{"$gt": "v"}},
		"in non-list":       {"k": map[string]interface{}{"$in": "v"}},
		"top-level bad op":  {"$bogus": []interface{}{}},
		"nested bad member": {"$and": []interface{}{"x"}},
	} {
		if _, err := translateFilter(filter); err == nil {
			t.Fatalf("%s should fail", name)
		}
	}
	if got, err := translateFilter(nil); err != nil || got != "" {
		t.Fatalf("empty filter: %q %v", got, err)
	}
}

// fakeVectorService pages the fixed vector set the way the engine does:
// next_token is the offset of the next row.
type fakeVectorService struct {
	vector_pb.VectorServiceClient
	vectors []*vector_pb.Vector
}

func (f *fakeVectorService) ListVectors(_ context.Context, in *vector_pb.ListVectorsRequest, _ ...grpc.CallOption) (*vector_pb.ListVectorsResponse, error) {
	offset := 0
	if in.NextToken != "" {
		var err error
		offset, err = strconv.Atoi(in.NextToken)
		if err != nil {
			return nil, err
		}
	}
	page := int(in.MaxResults)
	if page <= 0 {
		page = 500
	}
	end := offset + page
	if end > len(f.vectors) {
		end = len(f.vectors)
	}
	next := ""
	if end < len(f.vectors) {
		next = strconv.Itoa(end)
	}
	return &vector_pb.ListVectorsResponse{Vectors: f.vectors[offset:end], NextToken: next}, nil
}

func listSegment(t *testing.T, s *Server, req *ListVectorsRequest) (*ListVectorsResponse, int) {
	t.Helper()
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/ListVectors", nil)
	s.listSegmented(rec, r, req, vectorBucketARNs{bucket: "b", index: "i"})
	resp := &ListVectorsResponse{}
	_ = json.NewDecoder(rec.Body).Decode(resp)
	return resp, rec.Code
}

func TestListSegmented(t *testing.T) {
	vectors := make([]*vector_pb.Vector, 0, 10)
	for i := 0; i < 10; i++ {
		vectors = append(vectors, &vector_pb.Vector{Key: "k" + strconv.Itoa(i)})
	}
	s := &Server{vectors: &fakeVectorService{vectors: vectors}}

	keys := func(vs []*OutputVector) []string {
		out := make([]string, 0, len(vs))
		for _, v := range vs {
			out = append(out, v.Key)
		}
		return out
	}

	// Segment 0 of 2, page of 3: indices 0, 2, 4 then a resume token.
	resp, code := listSegment(t, s, &ListVectorsRequest{
		SegmentCount: 2, SegmentIndex: 0, MaxResults: 3,
	})
	if code != http.StatusOK {
		t.Fatalf("code %d", code)
	}
	if got := keys(resp.Vectors); len(got) != 3 || got[0] != "k0" || got[1] != "k2" || got[2] != "k4" {
		t.Fatalf("first page: %v", got)
	}
	if resp.NextToken == "" {
		t.Fatal("expected a nextToken")
	}
	resp, _ = listSegment(t, s, &ListVectorsRequest{
		SegmentCount: 2, SegmentIndex: 0, MaxResults: 3, NextToken: resp.NextToken,
	})
	if got := keys(resp.Vectors); len(got) != 2 || got[0] != "k6" || got[1] != "k8" {
		t.Fatalf("second page: %v", got)
	}
	if resp.NextToken != "" {
		t.Fatalf("expected no nextToken, got %q", resp.NextToken)
	}

	// Segment 1 of 2 collects the odd indices without a token.
	resp, _ = listSegment(t, s, &ListVectorsRequest{SegmentCount: 2, SegmentIndex: 1})
	if got := keys(resp.Vectors); len(got) != 5 || got[0] != "k1" || got[4] != "k9" {
		t.Fatalf("segment 1: %v", got)
	}
}

func TestListSegmentedRejects(t *testing.T) {
	s := &Server{vectors: &fakeVectorService{}}
	for _, req := range []*ListVectorsRequest{
		{SegmentCount: 0, SegmentIndex: 1},
		{SegmentCount: 2, SegmentIndex: 2},
		{SegmentCount: 17, SegmentIndex: 0},
		{SegmentCount: 2, SegmentIndex: 0, NextToken: "bogus"},
		{SegmentCount: 2, SegmentIndex: 0, NextToken: "seg:0:0"},
	} {
		if _, code := listSegment(t, s, req); code != http.StatusBadRequest {
			t.Fatalf("%+v: code %d, want 400", req, code)
		}
	}
}
