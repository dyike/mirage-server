#ifndef MIRAGE_MEDIACODEC_H
#define MIRAGE_MEDIACODEC_H
#include <stdint.h>
#include <stddef.h>
typedef struct MirageCodec MirageCodec;
typedef struct {
    uint8_t *pixels;
    int width, height;
    double conversion_ms;
    int64_t timestamp_ns;
} MiragePicture;
MirageCodec *mirage_codec_new(void);
int mirage_codec_configure(MirageCodec *, const char *, int, int,
                          const void *, size_t, const void *, size_t);
int mirage_codec_input(MirageCodec *, const void *, size_t, uint64_t);
int mirage_codec_drain(MirageCodec *);
int mirage_codec_output(MirageCodec *, MiragePicture *);
const char *mirage_codec_name(MirageCodec *);
const char *mirage_codec_error(MirageCodec *);
int mirage_codec_hardware(MirageCodec *);
void mirage_codec_delete(MirageCodec *);
#endif
