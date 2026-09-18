package store

import (
	"mime"
	"path/filepath"
	"strings"
)

// Kind 资源的粗分类（给清单/保留策略做筛选维度；不参与 key 与 URL）。
//
// 为什么单独有 pdf：家庭资源里 PDF 是一等公民（paper.read / PDF 投屏都靠它），
// 不能被并进 application/* 的 document 里。
type Kind string

const (
	KindImage    Kind = "image"
	KindAudio    Kind = "audio"
	KindVideo    Kind = "video"
	KindPDF      Kind = "pdf"
	KindText     Kind = "text"
	KindDocument Kind = "document"
	KindOther    Kind = "other"
)

// Kinds 固定顺序（统计/文档输出用；不要依赖 map 遍历顺序）。
var Kinds = []Kind{KindImage, KindAudio, KindVideo, KindPDF, KindText, KindDocument, KindOther}

// KindOf 由 MIME 归类。
func KindOf(mimeType string) Kind {
	m := strings.ToLower(strings.TrimSpace(mimeType))
	if i := strings.IndexByte(m, ';'); i > 0 {
		m = strings.TrimSpace(m[:i])
	}
	switch {
	case m == "" || m == "application/octet-stream":
		return KindOther
	case strings.HasPrefix(m, "image/"):
		return KindImage
	case strings.HasPrefix(m, "audio/"):
		return KindAudio
	case strings.HasPrefix(m, "video/"):
		return KindVideo
	case m == "application/pdf":
		return KindPDF
	case strings.HasPrefix(m, "text/"), m == "application/json", m == "application/xml",
		strings.HasSuffix(m, "+json"), strings.HasSuffix(m, "+xml"):
		return KindText
	case strings.HasPrefix(m, "application/"):
		return KindDocument
	default:
		return KindOther
	}
}

// extensionMime：我们关心的扩展名 → MIME 的显式映射。
//
// 为什么不直接靠 mime.TypeByExtension：Go 会去读宿主机的 mime.types（macOS/Linux/容器
// 各不同），同一个文件在不同机器上可能归到不同类型 —— 分类结果会随部署环境漂移。
// 显式表在前、系统表兜底，保证「同一份字节在哪台机器上都是同一个 kind」。
var extensionMime = map[string]string{
	".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".png": "image/png",
	".gif": "image/gif", ".webp": "image/webp", ".heic": "image/heic",
	".heif": "image/heic", ".bmp": "image/bmp", ".tif": "image/tiff", ".tiff": "image/tiff",
	".mp3": "audio/mpeg", ".m4a": "audio/mp4", ".aac": "audio/aac", ".wav": "audio/wav",
	".flac": "audio/flac", ".opus": "audio/opus", ".ogg": "audio/ogg",
	".mp4": "video/mp4", ".m4v": "video/mp4", ".mov": "video/quicktime",
	".webm": "video/webm", ".mkv": "video/x-matroska", ".avi": "video/x-msvideo",
	".pdf": "application/pdf",
	".txt": "text/plain", ".md": "text/markdown", ".markdown": "text/markdown",
	".csv": "text/csv", ".log": "text/plain", ".srt": "text/plain", ".vtt": "text/vtt",
	".json": "application/json", ".xml": "application/xml", ".yaml": "text/yaml", ".yml": "text/yaml",
	".doc":  "application/msword",
	".docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	".xls":  "application/vnd.ms-excel",
	".xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	".ppt":  "application/vnd.ms-powerpoint",
	".pptx": "application/vnd.openxmlformats-officedocument.presentationml.presentation",
	".epub": "application/epub+zip",
}

func mimeTypeOf(name string) string {
	ext := strings.ToLower(filepath.Ext(name))
	if ct, ok := extensionMime[ext]; ok {
		return ct
	}
	if ct := mime.TypeByExtension(ext); ct != "" {
		if i := strings.IndexByte(ct, ';'); i > 0 {
			return strings.TrimSpace(ct[:i])
		}
		return ct
	}
	return "application/octet-stream"
}
