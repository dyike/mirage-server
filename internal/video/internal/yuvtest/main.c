// Standalone correctness/throughput test, also runnable on Android via adb:
// clang -O2 -Wall -Wextra main.c -o yuvtest && ./yuvtest
#define _POSIX_C_SOURCE 200809L
#include "../../yuv420.h"
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>

static double now_ms(void) {
    struct timespec t;
    clock_gettime(CLOCK_MONOTONIC, &t);
    return t.tv_sec*1000.0 + t.tv_nsec/1e6;
}

static uint32_t random_state = 12345;
static uint8_t sample(void) {
    random_state ^= random_state << 13;
    random_state ^= random_state >> 17;
    random_state ^= random_state << 5;
    return (uint8_t)random_state;
}

static int check(void) {
    // Exact-size allocations catch vector loads past the final chroma sample
    // under ASan, including interleaved planes and odd crops/dimensions.
    const int widths[] = {1, 2, 7, 8, 9, 15, 16, 17, 31, 32, 67};
    for (int stride = 1; stride <= 3; stride++) {
        for (int left = 0; left < 3; left++) {
            for (int top = 0; top < 2; top++) {
                for (size_t wi = 0; wi < sizeof(widths)/sizeof(widths[0]); wi++) {
                    int w = widths[wi], h = 9, image_w = w + left, image_h = h + top;
                    for (int padding = 0; padding < 2; padding++) {
                        int32_t pixel[3] = {padding ? 2 : 1, stride, stride};
                        int32_t row[3] = {image_w*pixel[0]+padding*13,
                            ((image_w+1)/2)*stride+padding*7,
                            ((image_w+1)/2)*stride+padding*11};
                        const uint8_t *planes[3];
                        for (int i = 0; i < 3; i++) {
                            int ph = i ? (image_h+1)/2 : image_h;
                            int pw = i ? (image_w+1)/2 : image_w;
                            size_t size = (size_t)(ph-1)*row[i]+(pw-1)*pixel[i]+1;
                            uint8_t *data = malloc(size);
                            for (size_t n = 0; n < size; n++) data[n] = sample();
                            planes[i] = data;
                        }
                        size_t size = (size_t)w*h*4;
                        uint8_t *expected = malloc(size), *actual = malloc(size);
                        for (int full = 0; full <= 1; full++) {
                            for (int bt709 = 0; bt709 <= 1; bt709++) {
                                MirageYUVMatrix m = mirage_yuv_matrix(full, bt709);
                                mirage_yuv420_bgra_scalar(expected, planes, row, pixel, left, top, w, h, m);
                                memset(actual, 0, size);
                                mirage_yuv420_bgra(actual, planes, row, pixel, left, top, w, h, m);
                                if (memcmp(expected, actual, size)) {
                                    fprintf(stderr, "Mismatch: w=%d left=%d top=%d stride=%d padding=%d full=%d bt709=%d\n", w, left, top, stride, padding, full, bt709);
                                    return 1;
                                }
                            }
                        }
                        free(expected); free(actual);
                        for (int i = 0; i < 3; i++) free((void *)planes[i]);
                    }
                }
            }
        }
    }
    puts("PASS: cropped/odd/padded planar and interleaved YUV; BT.601/709 full/limited");
    return 0;
}

static void bench(int w, int h) {
    int32_t row[3] = {w, w, w}, pixel[3] = {1, 2, 2};
    size_t ysize = (size_t)w*h;
    uint8_t *y = malloc(ysize), *u = malloc(ysize/2), *v = malloc(ysize/2);
    uint8_t *dst = malloc(ysize*4);
    for (size_t n = 0; n < ysize; n++) y[n] = sample();
    for (size_t n = 0; n < ysize/2; n++) { u[n] = sample(); v[n] = sample(); }
    const uint8_t *planes[3] = {y, u, v};
    MirageYUVMatrix m = mirage_yuv_matrix(0, 1);
    unsigned checksum = 0;
    for (int fast = 0; fast <= 1; fast++) {
        double start = now_ms();
        for (int n = 0; n < 40; n++) {
            y[n]++;
            if (fast) mirage_yuv420_bgra(dst, planes, row, pixel, 0, 0, w, h, m);
            else mirage_yuv420_bgra_scalar(dst, planes, row, pixel, 0, 0, w, h, m);
            checksum += dst[n*4];
        }
        printf("%dx%d %s: %.2f ms/frame\n", w, h, fast ? "selected" : "scalar", (now_ms()-start)/40);
    }
    printf("checksum=%u\n", checksum);
    free(y); free(u); free(v); free(dst);
}

int main(int argc, char **argv) {
    if (check()) return 1;
    if (argc > 1 && !strcmp(argv[1], "--bench")) {
        bench(1920, 1080);
        bench(2560, 1440);
        bench(3840, 2160);
    }
    return 0;
}
