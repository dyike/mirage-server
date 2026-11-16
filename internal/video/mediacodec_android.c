//go:build android && cgo

#include "mediacodec_android.h"
#include "yuv420.h"
#include <media/NdkMediaCodec.h>
#include <media/NdkImageReader.h>
#include <dlfcn.h>
#include <pthread.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>

// Load APIs newer than our minimum SDK at runtime. Android 6 can still run
// Mirage using RFB; ImageReader video output needs Android 7 or later.
static struct {
    void *lib;
    media_status_t (*reader_new)(int32_t,int32_t,int32_t,int32_t,AImageReader**);
    void (*reader_delete)(AImageReader*);
    media_status_t (*reader_window)(AImageReader*,ANativeWindow**);
    media_status_t (*reader_acquire)(AImageReader*,AImage**);
    void (*image_delete)(AImage*);
    media_status_t (*image_width)(const AImage*,int32_t*);
    media_status_t (*image_height)(const AImage*,int32_t*);
    media_status_t (*image_crop)(const AImage*,AImageCropRect*);
    media_status_t (*image_timestamp)(const AImage*,int64_t*);
    media_status_t (*plane_data)(const AImage*,int,uint8_t**,int*);
    media_status_t (*plane_row)(const AImage*,int,int32_t*);
    media_status_t (*plane_pixel)(const AImage*,int,int32_t*);
    media_status_t (*codec_name)(AMediaCodec*,char**);
    void (*release_name)(AMediaCodec*,char*);
    media_status_t (*find_decoder)(const AMediaFormat*,const void**);
    int (*codec_type)(const void*);
    const char *(*canonical_name)(const void*);
    media_status_t (*codec_info)(const char*,const void**);
    int available;
} api;
static pthread_once_t api_once = PTHREAD_ONCE_INIT;

static void load_api(void) {
    api.lib = dlopen("libmediandk.so", RTLD_NOW | RTLD_LOCAL);
    if (!api.lib) return;
#define LOAD(field, symbol) *(void **)(&api.field) = dlsym(api.lib, symbol)
    LOAD(reader_new, "AImageReader_new");
    LOAD(reader_delete, "AImageReader_delete");
    LOAD(reader_window, "AImageReader_getWindow");
    LOAD(reader_acquire, "AImageReader_acquireLatestImage");
    LOAD(image_delete, "AImage_delete");
    LOAD(image_width, "AImage_getWidth");
    LOAD(image_height, "AImage_getHeight");
    LOAD(image_crop, "AImage_getCropRect");
    LOAD(image_timestamp, "AImage_getTimestamp");
    LOAD(plane_data, "AImage_getPlaneData");
    LOAD(plane_row, "AImage_getPlaneRowStride");
    LOAD(plane_pixel, "AImage_getPlanePixelStride");
    LOAD(codec_name, "AMediaCodec_getName");
    LOAD(release_name, "AMediaCodec_releaseName");
    LOAD(find_decoder, "AMediaCodecStore_findNextDecoderForFormat");
    LOAD(codec_type, "AMediaCodecInfo_getMediaCodecInfoType");
    LOAD(canonical_name, "AMediaCodecInfo_getCanonicalName");
    LOAD(codec_info, "AMediaCodecStore_getCodecInfo");
#undef LOAD
    api.available = api.reader_new && api.reader_delete && api.reader_window &&
        api.reader_acquire && api.image_delete && api.image_width && api.image_height &&
        api.image_crop && api.plane_data && api.plane_row && api.plane_pixel;
}

struct MirageCodec {
    AMediaCodec *codec;
    AImageReader *reader;
    uint8_t *pixels;
    size_t capacity;
    int started, hardware;
    pthread_mutex_t output_mu;
    ssize_t pending; // newest decoded buffer, owned until rendered or replaced
    MirageYUVMatrix matrix; // output format; guarded by output_mu
    char name[256];
    // Only written during configuration/input. Output errors are returned as
    // numeric codes so the two codec threads never race on this string.
    char error[256];
};

MirageCodec *mirage_codec_new(void) {
    pthread_once(&api_once, load_api);
    if (!api.available) return NULL;
    MirageCodec *d = calloc(1, sizeof(*d));
    if (d) {
        d->hardware = -1;
        d->pending = -1;
        d->matrix = mirage_yuv_matrix(0, 1); // limited BT.709 until format notification
        pthread_mutex_init(&d->output_mu, NULL);
    }
    return d;
}

static int fail(MirageCodec *d, const char *step, int status) {
    snprintf(d->error, sizeof(d->error), "MediaCodec %s: %d", step, status);
    return -1;
}

int mirage_codec_configure(MirageCodec *d, const char *mime, int w, int h,
                          const void *csd0, size_t size0, const void *csd1, size_t size1) {
    AMediaFormat *f = AMediaFormat_new();
    if (!f) return fail(d, "format allocation", -1);
    AMediaFormat_setString(f, "mime", mime);
    AMediaFormat_setInt32(f, "width", w);
    AMediaFormat_setInt32(f, "height", h);
    AMediaFormat_setInt32(f, "color-format", 0x7f420888); // YUV420Flexible
    AMediaFormat_setInt32(f, "low-latency", 1);
    AMediaFormat_setInt32(f, "max-input-size", 16 << 20);
    AMediaFormat_setBuffer(f, "csd-0", csd0, size0);
    if (size1) AMediaFormat_setBuffer(f, "csd-1", csd1, size1);

    // API 36+: select an explicitly advertised hardware decoder. On older
    // releases use the platform's preferred decoder and keep the hardware
    // state unknown unless we can query it, rather than guessing from its name.
    if (api.find_decoder && api.codec_type && api.canonical_name) {
        const void *info = NULL;
        while (api.find_decoder(f, &info) == AMEDIA_OK && info) {
            if (api.codec_type(info) != 2) continue; // HARDWARE_ACCELERATED
            const char *name = api.canonical_name(info);
            if (!name) continue;
            d->codec = AMediaCodec_createCodecByName(name);
            if (d->codec) { d->hardware = 1; break; }
        }
    }
    if (!d->codec) d->codec = AMediaCodec_createDecoderByType(mime);
    if (!d->codec) {
        AMediaFormat_delete(f);
        return fail(d, "create decoder", -1);
    }
    if (api.codec_name && api.release_name) {
        char *name = NULL;
        if (api.codec_name(d->codec, &name) == AMEDIA_OK && name) {
            snprintf(d->name, sizeof(d->name), "%s", name);
            if (api.codec_info && api.codec_type) {
                const void *info = NULL;
                if (api.codec_info(name, &info) == AMEDIA_OK && info)
                    d->hardware = api.codec_type(info) == 2;
            }
            api.release_name(d->codec, name);
        }
    }
    int status = api.reader_new(w, h, AIMAGE_FORMAT_YUV_420_888, 3, &d->reader);
    ANativeWindow *window = NULL;
    if (status == AMEDIA_OK) status = api.reader_window(d->reader, &window);
    if (status == AMEDIA_OK) status = AMediaCodec_configure(d->codec, f, window, NULL, 0);
    AMediaFormat_delete(f);
    if (status != AMEDIA_OK) return fail(d, "configure ImageReader", status);
    status = AMediaCodec_start(d->codec);
    if (status != AMEDIA_OK) return fail(d, "start", status);
    d->started = 1;
    return 0;
}

int mirage_codec_input(MirageCodec *d, const void *data, size_t size, uint64_t pts) {
    // Startup consumes input slots for SPS/PPS; allow the codec's initial
    // allocation to finish before treating temporary backpressure as a loss.
    ssize_t idx = AMediaCodec_dequeueInputBuffer(d->codec, 250000);
    if (idx < 0) return fail(d, "dequeue input", (int)idx);
    size_t capacity = 0;
    uint8_t *input = AMediaCodec_getInputBuffer(d->codec, (size_t)idx, &capacity);
    if (!input || size > capacity) {
        AMediaCodec_queueInputBuffer(d->codec, (size_t)idx, 0, 0, pts, 0);
        return fail(d, "input buffer too small", (int)capacity);
    }
    memcpy(input, data, size);
    int status = AMediaCodec_queueInputBuffer(d->codec, (size_t)idx, 0, size, pts, 0);
    return status == AMEDIA_OK ? 0 : fail(d, "queue input", status);
}

static int picture(MirageCodec *d, AImage *image, MiragePicture *out) {
    int32_t width = 0, height = 0;
    AImageCropRect crop;
    if (api.image_width(image, &width) || api.image_height(image, &height) ||
        api.image_crop(image, &crop)) return -1001;
    int w = crop.right - crop.left, h = crop.bottom - crop.top;
    if (crop.left < 0 || crop.top < 0 || w <= 0 || h <= 0 ||
        crop.right > width || crop.bottom > height || w > 16384 || h > 16384 ||
        (int64_t)w * h > (64 << 20)) return -1002;
    uint8_t *plane[3];
    int length[3];
    int32_t row[3], pixel[3];
    for (int i = 0; i < 3; i++) {
        if (api.plane_data(image, i, &plane[i], &length[i]) ||
            api.plane_row(image, i, &row[i]) || api.plane_pixel(image, i, &pixel[i]) ||
            !plane[i] || row[i] <= 0 || pixel[i] <= 0) return -1003;
        int shift = i ? 1 : 0;
        int64_t end = (int64_t)((crop.bottom-1)>>shift)*row[i] +
                      (int64_t)((crop.right-1)>>shift)*pixel[i];
        if (end >= length[i]) return -1004;
    }
    size_t size = (size_t)w * h * 4;
    if (size > d->capacity) {
        uint8_t *p = realloc(d->pixels, size);
        if (!p) return -1005;
        d->pixels = p;
        d->capacity = size;
    }
    // ImageReader exposes actual plane strides, including interleaved chroma;
    // never assume that a vendor's flexible YUV buffer is tightly packed NV12.
    pthread_mutex_lock(&d->output_mu);
    MirageYUVMatrix matrix = d->matrix;
    pthread_mutex_unlock(&d->output_mu);
    const uint8_t *planes[3] = {plane[0], plane[1], plane[2]};
    struct timespec start, end;
    clock_gettime(CLOCK_MONOTONIC, &start);
    mirage_yuv420_bgra(d->pixels, planes, row, pixel, crop.left, crop.top, w, h,
        matrix);
    clock_gettime(CLOCK_MONOTONIC, &end);
    out->conversion_ms = (end.tv_sec-start.tv_sec)*1000.0 + (end.tv_nsec-start.tv_nsec)/1e6;
    out->pixels = d->pixels; out->width = w; out->height = h;
    return 1;
}

// Drain independently of CPU conversion and the framebuffer callback. Keeping
// exactly one pending decoded buffer allows input/reference decoding to proceed
// while a slow renderer works, and preserves the final frame of a still screen.
int mirage_codec_drain(MirageCodec *d) {
    AMediaCodecBufferInfo info;
    ssize_t idx = AMediaCodec_dequeueOutputBuffer(d->codec, &info, 10000);
    if (idx >= 0) {
        pthread_mutex_lock(&d->output_mu);
        ssize_t previous = d->pending;
        d->pending = idx;
        pthread_mutex_unlock(&d->output_mu);
        if (previous >= 0)
            return AMediaCodec_releaseOutputBuffer(d->codec, (size_t)previous, false);
    } else if (idx == AMEDIACODEC_INFO_OUTPUT_FORMAT_CHANGED) {
        // Query metadata only when it changes, rather than allocating and
        // crossing into the codec for every converted picture.
        AMediaFormat *format = AMediaCodec_getOutputFormat(d->codec);
        int32_t standard = 1, range = 2;
        if (format) {
            AMediaFormat_getInt32(format, "color-standard", &standard);
            AMediaFormat_getInt32(format, "color-range", &range);
            AMediaFormat_delete(format);
        }
        pthread_mutex_lock(&d->output_mu);
        d->matrix = mirage_yuv_matrix(range == 1, standard == 1);
        pthread_mutex_unlock(&d->output_mu);
    } else if (idx < AMEDIACODEC_INFO_OUTPUT_BUFFERS_CHANGED) {
        return (int)idx;
    }
    return 0;
}

int mirage_codec_output(MirageCodec *d, MiragePicture *out) {
    pthread_mutex_lock(&d->output_mu);
    ssize_t idx = d->pending;
    d->pending = -1;
    pthread_mutex_unlock(&d->output_mu);
    if (idx >= 0) {
        int status = AMediaCodec_releaseOutputBuffer(d->codec, (size_t)idx, true);
        if (status != AMEDIA_OK) return status;
    }
    AImage *image = NULL;
    int status = api.reader_acquire(d->reader, &image);
    if (status == AMEDIA_IMGREADER_NO_BUFFER_AVAILABLE) return 0;
    if (status != AMEDIA_OK || !image) return status ? status : -1006;
    int result = picture(d, image, out);
    out->timestamp_ns = 0;
    if (api.image_timestamp) api.image_timestamp(image, &out->timestamp_ns);
    api.image_delete(image);
    return result;
}

const char *mirage_codec_name(MirageCodec *d) { return d->name; }
const char *mirage_codec_error(MirageCodec *d) { return d->error; }
int mirage_codec_hardware(MirageCodec *d) { return d->hardware; }
void mirage_codec_delete(MirageCodec *d) {
    if (!d) return;
    if (d->codec) {
        if (d->started) AMediaCodec_stop(d->codec);
        AMediaCodec_delete(d->codec);
    }
    if (d->reader) api.reader_delete(d->reader);
    pthread_mutex_destroy(&d->output_mu);
    free(d->pixels);
    free(d);
}
