// C interface to WebRTC's audio processing (AEC3 echo canceller), for cgo:
// see bbaec.go. 16-bit mono, 10 ms frames.
#ifndef BBAEC_H_
#define BBAEC_H_

#include <stdint.h>

#ifdef __cplusplus
extern "C" {
#endif

typedef struct bb_aec bb_aec;

bb_aec *bb_aec_new(int rate);
// The far end (what the echo is made of), 10 ms. 0 on success.
int bb_aec_render(bb_aec *a, int16_t *frame);
// The signal with the echo in it, 10 ms, cleaned in place. 0 on success.
int bb_aec_capture(bb_aec *a, int16_t *frame);
void bb_aec_free(bb_aec *a);

#ifdef __cplusplus
}
#endif

#endif
