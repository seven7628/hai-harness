package agenthub

// 附件协议：Validate 边界（数量/图片总量/类型/data URL/元数据）+ 错误都包裹 ErrInvalid。

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func testImageAttachment(mime string) Attachment {
	return Attachment{
		Kind: AttachmentKindImage, Name: "shot.png", MIME: mime,
		Bytes: 3, DataURL: "data:" + mime + ";base64,QUJD",
	}
}

func TestAttachmentValidateKinds(t *testing.T) {
	cases := []struct {
		name string
		att  Attachment
		ok   bool
	}{
		{"image png", testImageAttachment("image/png"), true},
		{"image jpeg", testImageAttachment("image/jpeg"), true},
		{"image webp", testImageAttachment("image/webp"), true},
		{"image gif", testImageAttachment("image/gif"), true},
		{"image 无显式 mime", Attachment{Kind: AttachmentKindImage, DataURL: "data:image/png;base64,QUJD"}, true},
		{"image http URL", Attachment{Kind: AttachmentKindImage, DataURL: "https://example.com/a.png"}, false},
		{"image 非图片 data URL", Attachment{Kind: AttachmentKindImage, DataURL: "data:text/plain;base64,QUJD"}, false},
		{"image 不支持的类型", Attachment{Kind: AttachmentKindImage, DataURL: "data:image/bmp;base64,QUJD"}, false},
		{"image 空负载", Attachment{Kind: AttachmentKindImage, DataURL: "data:image/png;base64,"}, false},
		{"image 坏 base64", Attachment{Kind: AttachmentKindImage, DataURL: "data:image/png;base64,QU$D"}, false},
		{"image base64 长度 mod1", Attachment{Kind: AttachmentKindImage, DataURL: "data:image/png;base64,QUJD="}, false},
		{"image base64 只有填充", Attachment{Kind: AttachmentKindImage, DataURL: "data:image/png;base64,=="}, false},
		{"image base64 无填充", Attachment{Kind: AttachmentKindImage, DataURL: "data:image/png;base64,QUJ"}, true},
		{"image base64 填充后还有数据", Attachment{Kind: AttachmentKindImage, DataURL: "data:image/png;base64,QU=J"}, false},
		{"image 缺 data_url", Attachment{Kind: AttachmentKindImage, Name: "a.png"}, false},
		{"image mime 与 data URL 不一致", func() Attachment {
			a := testImageAttachment("image/png")
			a.MIME = "image/jpeg"
			return a
		}(), false},
		{"image 带 path", func() Attachment {
			a := testImageAttachment("image/png")
			a.Path = "/tmp/a.png"
			return a
		}(), false},
		{"file 只有 name", Attachment{Kind: AttachmentKindFile, Name: "report.pdf"}, true},
		{"file 只有 path", Attachment{Kind: AttachmentKindFile, Path: "/tmp/report.pdf"}, true},
		{"file name/path 都空", Attachment{Kind: AttachmentKindFile}, false},
		{"file 带 data_url", Attachment{Kind: AttachmentKindFile, Name: "a.bin", DataURL: "data:image/png;base64,QUJD"}, false},
		{"kind 为空", Attachment{Name: "a"}, false},
		{"kind 非法", Attachment{Kind: "video", Name: "a"}, false},
		{"bytes 为负", Attachment{Kind: AttachmentKindFile, Name: "a", Bytes: -1}, false},
	}
	for _, tc := range cases {
		err := tc.att.Validate()
		if tc.ok && err != nil {
			t.Errorf("%s: err = %v, want nil", tc.name, err)
		}
		if !tc.ok {
			if err == nil {
				t.Errorf("%s: err = nil, want ErrInvalid", tc.name)
			} else if !errors.Is(err, ErrInvalid) {
				t.Errorf("%s: err = %v, want wraps ErrInvalid", tc.name, err)
			}
		}
	}
}

func TestAttachmentValidateNamePathLength(t *testing.T) {
	img := testImageAttachment("image/png")
	img.Name = strings.Repeat("a", MaxAttachmentNameLen)
	if err := img.Validate(); err != nil {
		t.Fatalf("name 恰好 %d 字节应通过: %v", MaxAttachmentNameLen, err)
	}
	img.Name = strings.Repeat("a", MaxAttachmentNameLen+1)
	if err := img.Validate(); !errors.Is(err, ErrInvalid) {
		t.Fatalf("超长 name err = %v, want ErrInvalid", err)
	}
	// 上限是字节数（协议/JSON 体积口径），不是字符数：171 个汉字 = 513 字节。
	img.Name = strings.Repeat("图", 170)
	if err := img.Validate(); err != nil {
		t.Fatalf("170 个汉字（510 字节）应通过: %v", err)
	}
	img.Name = strings.Repeat("图", 171)
	if err := img.Validate(); !errors.Is(err, ErrInvalid) {
		t.Fatalf("171 个汉字（513 字节）err = %v, want ErrInvalid", err)
	}
	file := Attachment{Kind: AttachmentKindFile, Name: "f"}
	file.Path = "/" + strings.Repeat("p", MaxAttachmentPathLen-1)
	if err := file.Validate(); err != nil {
		t.Fatalf("path 恰好 %d 字节应通过: %v", MaxAttachmentPathLen, err)
	}
	file.Path += "p"
	if err := file.Validate(); !errors.Is(err, ErrInvalid) {
		t.Fatalf("超长 path err = %v, want ErrInvalid", err)
	}
}

// dataURLOfSize 造一个总字节数恰为 size 的合法 data URL。
func dataURLOfSize(t *testing.T, size int) string {
	t.Helper()
	const prefix = "data:image/png;base64,"
	if size <= len(prefix) {
		t.Fatalf("size %d 太小", size)
	}
	return prefix + strings.Repeat("A", size-len(prefix))
}

func TestValidateAttachmentsCount(t *testing.T) {
	file := func() Attachment { return Attachment{Kind: AttachmentKindFile, Name: "f"} }
	ok := make([]Attachment, MaxMessageAttachments)
	for i := range ok {
		ok[i] = file()
	}
	if err := ValidateAttachments(ok); err != nil {
		t.Fatalf("%d 个附件应通过: %v", MaxMessageAttachments, err)
	}
	over := append(ok, file())
	if err := ValidateAttachments(over); !errors.Is(err, ErrInvalid) {
		t.Fatalf("%d 个附件 err = %v, want ErrInvalid", len(over), err)
	}
}

func TestValidateAttachmentsImageBudget(t *testing.T) {
	fit := []Attachment{{Kind: AttachmentKindImage, DataURL: dataURLOfSize(t, MaxAttachmentImageBytes)}}
	if err := ValidateAttachments(fit); err != nil {
		t.Fatalf("恰好 %d 字节应通过: %v", MaxAttachmentImageBytes, err)
	}
	over := []Attachment{{Kind: AttachmentKindImage, DataURL: dataURLOfSize(t, MaxAttachmentImageBytes+1)}}
	if err := ValidateAttachments(over); !errors.Is(err, ErrInvalid) {
		t.Fatalf("超 1 字节 err = %v, want ErrInvalid", err)
	}
	// 多图合计超限同样拒绝（总量闸门，不是单图闸门）。
	half := MaxAttachmentImageBytes/2 + 1
	multi := []Attachment{
		{Kind: AttachmentKindImage, DataURL: dataURLOfSize(t, half)},
		{Kind: AttachmentKindImage, DataURL: dataURLOfSize(t, half)},
	}
	if err := ValidateAttachments(multi); !errors.Is(err, ErrInvalid) {
		t.Fatalf("多图合计超限 err = %v, want ErrInvalid", err)
	}
}

func TestValidateAttachmentsWrapsItemError(t *testing.T) {
	atts := []Attachment{
		{Kind: AttachmentKindFile, Name: "ok.txt"},
		{Kind: AttachmentKindImage, DataURL: "http://example.com/a.png"},
	}
	err := ValidateAttachments(atts)
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v, want wraps ErrInvalid", err)
	}
	if !strings.Contains(err.Error(), "附件 2") {
		t.Fatalf("err = %v, want 指明第 2 个附件", err)
	}
}

func TestAttachmentJSONShape(t *testing.T) {
	a := Attachment{Kind: AttachmentKindImage, Name: "a.png", MIME: "image/png", Bytes: 3, DataURL: "data:image/png;base64,QUJD"}
	raw, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	// 协议字段名必须与前端约定一致（kind/name/mime_type/bytes/data_url/path）。
	for _, key := range []string{`"kind"`, `"name"`, `"mime_type"`, `"bytes"`, `"data_url"`} {
		if !strings.Contains(string(raw), key) {
			t.Fatalf("JSON 缺字段 %s: %s", key, raw)
		}
	}
	var back Attachment
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back != a {
		t.Fatalf("roundtrip = %+v, want %+v", back, a)
	}
	file := Attachment{Kind: AttachmentKindFile, Name: "r.pdf", Path: "/tmp/r.pdf", Bytes: 9}
	raw, err = json.Marshal(file)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"path"`) {
		t.Fatalf("file JSON 缺 path: %s", raw)
	}
}
