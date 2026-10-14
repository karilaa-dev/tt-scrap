//go:build cgo

package media

/*
#cgo pkg-config: libheif
#include <libheif/heif.h>
#include <stdlib.h>

static struct heif_error decode_image(struct heif_image_handle *handle, struct heif_image **out) {
 struct heif_decoding_options *options = heif_decoding_options_alloc();
 if (options) options->convert_hdr_to_8bit = 1;
 struct heif_error result = heif_decode_image(handle, out, heif_colorspace_RGB, heif_chroma_interleaved_RGBA, options);
 heif_decoding_options_free(options);
 return result;
}
*/
import "C"

import (
	"fmt"
	"image"
	"unsafe"
)

func decodeHEIF(path string) (image.Image, error) {
	ctx := C.heif_context_alloc()
	if ctx == nil {
		return nil, fmt.Errorf("cannot allocate HEIF decoder")
	}
	defer C.heif_context_free(ctx)
	C.heif_context_set_maximum_image_size_limit(ctx, 10000)
	C.heif_context_set_max_decoding_threads(ctx, 1)
	filename := C.CString(path)
	defer C.free(unsafe.Pointer(filename))
	if result := C.heif_context_read_from_file(ctx, filename, nil); result.code != C.heif_error_Ok {
		return nil, fmt.Errorf("cannot read HEIF image (%d)", result.code)
	}
	var handle *C.struct_heif_image_handle
	if result := C.heif_context_get_primary_image_handle(ctx, &handle); result.code != C.heif_error_Ok {
		return nil, fmt.Errorf("HEIF primary image missing (%d)", result.code)
	}
	defer C.heif_image_handle_release(handle)
	w, h := int(C.heif_image_handle_get_width(handle)), int(C.heif_image_handle_get_height(handle))
	if w <= 0 || h <= 0 || int64(w)*int64(h) > maxPixels {
		return nil, fmt.Errorf("HEIF image dimensions exceed limit")
	}
	var decoded *C.struct_heif_image
	if result := C.decode_image(handle, &decoded); result.code != C.heif_error_Ok {
		return nil, fmt.Errorf("cannot decode HEIF image (%d)", result.code)
	}
	defer C.heif_image_release(decoded)
	var stride C.int
	plane := C.heif_image_get_plane_readonly(decoded, C.heif_channel_interleaved, &stride)
	if plane == nil || int(stride) < w*4 {
		return nil, fmt.Errorf("HEIF pixel plane missing")
	}
	data := unsafe.Slice((*byte)(unsafe.Pointer(plane)), int(stride)*h)
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		copy(img.Pix[y*img.Stride:y*img.Stride+w*4], data[y*int(stride):y*int(stride)+w*4])
	}
	return img, nil
}
