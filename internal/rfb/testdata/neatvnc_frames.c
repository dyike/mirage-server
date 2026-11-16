// Synthetic animated desktop for independent Tight/ZRLE interoperability tests.
// Requires NeatVNC 1.x development headers; never captures a real desktop.
#include <neatvnc.h>
#include <aml.h>
#include <drm_fourcc.h>
#include <stdint.h>
#include <stdlib.h>
#include <time.h>
static uint64_t now_us(void) {
 struct timespec ts; clock_gettime(CLOCK_MONOTONIC, &ts);
 return (uint64_t)ts.tv_sec*1000000+ts.tv_nsec/1000;
}
int main(int argc, char **argv) {
 if(argc!=2)return 2;
 struct aml *loop=aml_new(); aml_set_default(loop); aml_require_workers(loop,2);
 struct nvnc *server=nvnc_new();
 if(nvnc_listen_tcp(server,"127.0.0.1",atoi(argv[1]),NVNC_STREAM_NORMAL)<0)return 3;
 struct nvnc_display *display=nvnc_display_new(0,0); nvnc_add_display(server,display);
 uint64_t next=0,until=now_us()+10000000; unsigned frame_number=0;
 while(now_us()<until) {
  uint64_t now=now_us();
  if(now>=next) {
   struct nvnc_frame *frame=nvnc_frame_new(320,240,DRM_FORMAT_XBGR8888,320);
   uint8_t *pixels=nvnc_frame_get_addr(frame);
   int stride=nvnc_frame_get_stride(frame)*4;
   for(int y=0;y<240;y++)for(int x=0;x<320;x++) {
    uint8_t *p=pixels+y*stride+x*4;
    p[0]=30+x/2; p[1]=40+y/2; p[2]=90; p[3]=255;
    if(y<40 && x/20==(int)(frame_number%16)) {p[0]=220;p[1]=180;p[2]=40;}
   }
   nvnc_frame_set_pts(frame,now); nvnc_display_feed_frame(display,frame);nvnc_frame_unref(frame);
   frame_number++;next=now+33333;
  }
  aml_poll(loop,5000);aml_dispatch(loop);
 }
 nvnc_del(server);nvnc_display_unref(display);return 0;
}
