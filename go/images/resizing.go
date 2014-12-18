package images

import (
	"bytes"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"

	"github.com/disintegration/imaging"
)

func Resized(ext string, data []byte, width, height int) (resized []byte, w int, h int) {
	if width == 0 && height == 0 {
		return data, 0, 0
	}
	if srcImage, _, err := image.Decode(bytes.NewReader(data)); err == nil {
		bounds := srcImage.Bounds()
		var dstImage *image.NRGBA
		if bounds.Dx() > width && width != 0 || bounds.Dy() > height && height != 0 {
			if width == height && bounds.Dx() != bounds.Dy() {
				dstImage = imaging.Thumbnail(srcImage, width, height, imaging.Lanczos)
				w, h = width, height
			} else {
				dstImage = imaging.Resize(srcImage, width, height, imaging.Lanczos)
			}
		} else {
			return data, bounds.Dx(), bounds.Dy()
		}
		var buf bytes.Buffer
		switch ext {
		case ".png":
			png.Encode(&buf, dstImage)
		case ".jpg", ".jpeg":
			jpeg.Encode(&buf, dstImage, nil)
		case ".gif":
			gif.Encode(&buf, dstImage, nil)
		}
		return buf.Bytes(), dstImage.Bounds().Dx(), dstImage.Bounds().Dy()
	}
	return data, 0, 0
}
