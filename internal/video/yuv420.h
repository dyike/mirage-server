#ifndef MIRAGE_YUV420_H
#define MIRAGE_YUV420_H

#include <stdint.h>
#include <stddef.h>
#include <string.h>
#if defined(__aarch64__) && !defined(MIRAGE_YUV_SCALAR_ONLY)
#include <arm_neon.h>
#endif

typedef struct {
    int cy, bias, cr, cb, cgu, cgv;
} MirageYUVMatrix;

static inline MirageYUVMatrix mirage_yuv_matrix(int full, int bt709) {
    return (MirageYUVMatrix){
        full ? 256 : 298, full ? 0 : 16,
        bt709 ? (full ? 403 : 459) : (full ? 359 : 409),
        bt709 ? (full ? 475 : 541) : (full ? 454 : 516),
        bt709 ? (full ? 48 : 55) : (full ? 88 : 100),
        bt709 ? (full ? 120 : 136) : (full ? 183 : 208)
    };
}

static inline uint8_t mirage_yuv_clamp(int n) {
    return n < 0 ? 0 : n > 255 ? 255 : (uint8_t)n;
}

static inline void mirage_yuv_pixel(uint8_t *dst, int y, int u, int v, MirageYUVMatrix m) {
    int yy = m.cy * (y - m.bias);
    u -= 128;
    v -= 128;
    dst[0] = mirage_yuv_clamp((yy + m.cb*u + 128) >> 8);
    dst[1] = mirage_yuv_clamp((yy - m.cgu*u - m.cgv*v + 128) >> 8);
    dst[2] = mirage_yuv_clamp((yy + m.cr*v + 128) >> 8);
    dst[3] = 255;
}

#if defined(__aarch64__) && !defined(MIRAGE_YUV_SCALAR_ONLY)
static inline uint8x8_t mirage_chroma4(const uint8_t *p, int stride, int extra) {
    uint8x8_t samples;
    if (stride == 1) {
        uint32_t packed;
        memcpy(&packed, p, sizeof(packed));
        samples = vreinterpret_u8_u64(vcreate_u64(packed));
    } else if (stride == 2 && extra) {
        // ImageReader memory may be uncached. A single contiguous load is
        // much cheaper than four byte reads from the mapped decoder buffer.
        // Only use it when another chroma sample guarantees byte 7 exists.
        uint8x8_t packed = vld1_u8(p);
        samples = vuzp_u8(packed, packed).val[0];
    } else {
        samples = vdup_n_u8(0);
        samples = vset_lane_u8(p[0], samples, 0);
        samples = vset_lane_u8(p[stride], samples, 1);
        samples = vset_lane_u8(p[2*stride], samples, 2);
        samples = vset_lane_u8(p[3*stride], samples, 3);
    }
    return vzip_u8(samples, samples).val[0];
}
#endif

// Plane pointers address the full image. Crop offsets and vendor row/pixel
// strides are applied before sampling; chroma may be planar or interleaved.
static void mirage_yuv420_bgra_scalar(uint8_t *dst, const uint8_t *plane[3],
        const int32_t row[3], const int32_t pixel[3], int left, int top,
        int w, int h, MirageYUVMatrix m) {
    for (int y = 0; y < h; y++) {
        int sy = y + top;
        for (int x = 0; x < w; x++) {
            int sx = x + left;
            mirage_yuv_pixel(dst + ((size_t)y*w+x)*4,
                plane[0][sy*row[0]+sx*pixel[0]],
                plane[1][(sy/2)*row[1]+(sx/2)*pixel[1]],
                plane[2][(sy/2)*row[2]+(sx/2)*pixel[2]], m);
        }
    }
}

static void mirage_yuv420_bgra(uint8_t *dst, const uint8_t *plane[3],
        const int32_t row[3], const int32_t pixel[3], int left, int top,
        int w, int h, MirageYUVMatrix m) {
#if defined(__aarch64__) && !defined(MIRAGE_YUV_SCALAR_ONLY)
    if (pixel[0] == 1) {
        for (int y = 0; y < h; y++) {
            int sy = y + top;
            const uint8_t *yp = plane[0] + sy*row[0] + left;
            const uint8_t *up = plane[1] + (sy/2)*row[1];
            const uint8_t *vp = plane[2] + (sy/2)*row[2];
            uint8_t *out = dst + (size_t)y*w*4;
            int x = 0;
            // Align pairs to the full image's chroma grid, not the crop.
            if ((left & 1) && w > 0) {
                mirage_yuv_pixel(out, yp[0], up[(left/2)*pixel[1]], vp[(left/2)*pixel[2]], m);
                x = 1;
            }
            for (; x + 8 <= w; x += 8) {
                const uint8_t *u = up + ((left+x)/2)*pixel[1];
                const uint8_t *v = vp + ((left+x)/2)*pixel[2];
                // Keep the final block within tightly sized plane bounds.
                uint8x8_t us = mirage_chroma4(u, pixel[1], x+8 < w);
                uint8x8_t vs = mirage_chroma4(v, pixel[2], x+8 < w);
                int16x8_t yy = vsubq_s16(vreinterpretq_s16_u16(vmovl_u8(vld1_u8(yp+x))), vdupq_n_s16(m.bias));
                int16x8_t uu = vsubq_s16(vreinterpretq_s16_u16(vmovl_u8(us)), vdupq_n_s16(128));
                int16x8_t vv = vsubq_s16(vreinterpretq_s16_u16(vmovl_u8(vs)), vdupq_n_s16(128));
                int32x4_t yl = vmull_n_s16(vget_low_s16(yy), m.cy);
                int32x4_t yh = vmull_n_s16(vget_high_s16(yy), m.cy);
                int32x4_t bl = vmlal_n_s16(yl, vget_low_s16(uu), m.cb);
                int32x4_t bh = vmlal_n_s16(yh, vget_high_s16(uu), m.cb);
                int32x4_t rl = vmlal_n_s16(yl, vget_low_s16(vv), m.cr);
                int32x4_t rh = vmlal_n_s16(yh, vget_high_s16(vv), m.cr);
                int32x4_t gl = vmlsl_n_s16(vmlsl_n_s16(yl, vget_low_s16(uu), m.cgu), vget_low_s16(vv), m.cgv);
                int32x4_t gh = vmlsl_n_s16(vmlsl_n_s16(yh, vget_high_s16(uu), m.cgu), vget_high_s16(vv), m.cgv);
                uint8x8x4_t bgra = {{
                    vqmovun_s16(vcombine_s16(vrshrn_n_s32(bl, 8), vrshrn_n_s32(bh, 8))),
                    vqmovun_s16(vcombine_s16(vrshrn_n_s32(gl, 8), vrshrn_n_s32(gh, 8))),
                    vqmovun_s16(vcombine_s16(vrshrn_n_s32(rl, 8), vrshrn_n_s32(rh, 8))),
                    vdup_n_u8(255)
                }};
                vst4_u8(out+x*4, bgra);
            }
            for (; x < w; x++) {
                int sx = left+x;
                mirage_yuv_pixel(out+x*4, yp[x], up[(sx/2)*pixel[1]], vp[(sx/2)*pixel[2]], m);
            }
        }
        return;
    }
#endif
    mirage_yuv420_bgra_scalar(dst, plane, row, pixel, left, top, w, h, m);
}
#endif
