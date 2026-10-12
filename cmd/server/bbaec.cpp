// WebRTC audio processing (webrtc-audio-processing-1) behind a C interface.
#include "bbaec.h"

#include <api/scoped_refptr.h>
#include <modules/audio_processing/include/audio_processing.h>

struct bb_aec {
    rtc::scoped_refptr<webrtc::AudioProcessing> apm; // ref-counted: released with the struct
    webrtc::StreamConfig config;
};

extern "C" bb_aec *bb_aec_new(int rate)
{
    rtc::scoped_refptr<webrtc::AudioProcessing> apm(webrtc::AudioProcessingBuilder().Create());
    if (!apm) return nullptr;
    webrtc::AudioProcessing::Config c;
    c.echo_canceller.enabled = true;
    c.echo_canceller.mobile_mode = false; // AEC3, the full canceller
    c.high_pass_filter.enabled = true;
    c.noise_suppression.enabled = false;  // the peer's voice as it is
    c.gain_controller1.enabled = false;   // the gateway levels it afterwards
    c.gain_controller2.enabled = false;
    apm->ApplyConfig(c);
    apm->Initialize();
    return new bb_aec{apm, webrtc::StreamConfig(rate, 1)};
}

extern "C" int bb_aec_render(bb_aec *a, int16_t *frame)
{
    return a->apm->ProcessReverseStream(frame, a->config, a->config, frame);
}

extern "C" int bb_aec_capture(bb_aec *a, int16_t *frame)
{
    // Render and capture are fed already aligned (bbecho.go), with a fixed
    // lead of the render: the canceller's own estimator finds that.
    a->apm->set_stream_delay_ms(0);
    return a->apm->ProcessStream(frame, a->config, a->config, frame);
}

extern "C" void bb_aec_free(bb_aec *a)
{
    delete a;
}
