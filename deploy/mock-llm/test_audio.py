"""The voice routes: what bifrost forwards for read-aloud and dictation.

Run from this directory: `python3 -m unittest test_audio`.
"""
import json
import struct
import threading
import unittest
import urllib.error
import urllib.request
import uuid
from http.server import ThreadingHTTPServer

from server import TRANSCRIPT_TEXT, Handler, _wav


def multipart(fields: dict, filename: str, audio: bytes) -> tuple[bytes, str]:
    """The body bifrost's OpenAI-compatible transcription call sends."""
    boundary = uuid.uuid4().hex
    parts = []
    for name, value in fields.items():
        parts.append(
            f'--{boundary}\r\nContent-Disposition: form-data; name="{name}"\r\n\r\n{value}\r\n'.encode()
        )
    parts.append(
        (f'--{boundary}\r\nContent-Disposition: form-data; name="file"; filename="{filename}"\r\n'
         "Content-Type: audio/wav\r\n\r\n").encode() + audio + b"\r\n"
    )
    parts.append(f"--{boundary}--\r\n".encode())
    return b"".join(parts), f"multipart/form-data; boundary={boundary}"


class AudioRoutesTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        cls.thread = threading.Thread(target=cls.server.serve_forever)
        cls.thread.start()

    @classmethod
    def tearDownClass(cls):
        cls.server.shutdown()
        cls.server.server_close()
        cls.thread.join()

    def post(self, path, body, content_type):
        request = urllib.request.Request(
            f"http://127.0.0.1:{self.server.server_port}{path}",
            data=body,
            headers={"Content-Type": content_type, "Authorization": "Bearer mock-key-voice"},
        )
        try:
            with urllib.request.urlopen(request, timeout=5) as response:
                return response.status, response.headers.get("Content-Type"), response.read()
        except urllib.error.HTTPError as err:
            return err.code, err.headers.get("Content-Type"), err.read()

    def journal(self):
        with urllib.request.urlopen(f"http://127.0.0.1:{self.server.server_port}/__journal", timeout=5) as response:
            return json.loads(response.read())["data"]

    def test_speech_answers_a_wav_as_long_as_the_text(self):
        for path in ("/v1/audio/speech", "/openai/v1/audio/speech"):
            with self.subTest(path=path):
                short = self.post(path, json.dumps({"model": "tts", "input": "Hi."}).encode(), "application/json")
                long = self.post(path, json.dumps({"model": "tts", "input": "Hi. " * 20}).encode(), "application/json")
                self.assertEqual(short[0], 200)
                self.assertEqual(short[1], "audio/wav")
                self.assertEqual(short[2][:4], b"RIFF")
                self.assertEqual(short[2][8:12], b"WAVE")
                self.assertGreater(len(long[2]), len(short[2]))
                # Not silence: a decoder must schedule real samples.
                samples = struct.unpack_from("<100h", short[2], 44 + 200)
                self.assertTrue(any(samples))

    def test_speech_pcm_is_headerless(self):
        status, content_type, body = self.post(
            "/v1/audio/speech",
            json.dumps({"model": "tts", "input": "Hello", "response_format": "pcm"}).encode(),
            "application/json",
        )
        self.assertEqual(status, 200)
        self.assertTrue(content_type.startswith("audio/L16"))
        self.assertNotEqual(body[:4], b"RIFF")
        self.assertEqual(len(body) % 2, 0)

    def test_speech_refuses_an_empty_input_and_honours_error_markers(self):
        status, _, _ = self.post("/v1/audio/speech", json.dumps({"model": "tts"}).encode(), "application/json")
        self.assertEqual(status, 400)
        status, _, body = self.post(
            "/v1/audio/speech",
            json.dumps({"model": "tts", "input": "[[mock:http_429]] slow"}).encode(),
            "application/json",
        )
        self.assertEqual(status, 429)
        self.assertEqual(json.loads(body)["error"]["type"], "rate_limit_error")

    def test_transcription_answers_the_fixed_text_with_duration_usage(self):
        audio = _wav(b"\x00\x00" * 24000)  # one second at 24 kHz
        for path in ("/v1/audio/transcriptions", "/openai/deployments/whisper/audio/transcriptions"):
            with self.subTest(path=path):
                body, content_type = multipart({"model": "whisper-1", "language": "en"}, "speech.wav", audio)
                status, response_type, raw = self.post(path, body, content_type)
                self.assertEqual(status, 200)
                self.assertEqual(response_type, "application/json")
                payload = json.loads(raw)
                self.assertEqual(payload["text"], TRANSCRIPT_TEXT)
                self.assertEqual(payload["usage"], {"type": "duration", "seconds": 1.0})
        entry = self.journal()[-1]
        self.assertEqual(entry["model"], "whisper-1")
        self.assertEqual(entry["filename"], "speech.wav")
        self.assertEqual(entry["language"], "en")
        self.assertEqual(entry["credential"], "mock-key-voice")
        self.assertEqual(entry["audio_seconds"], 1.0)

    def test_transcription_text_format_is_plain_text(self):
        body, content_type = multipart({"model": "whisper-1", "response_format": "text"}, "a.wav", _wav(b"\x01\x00" * 10))
        status, response_type, raw = self.post("/v1/audio/transcriptions", body, content_type)
        self.assertEqual(status, 200)
        self.assertTrue(response_type.startswith("text/plain"))
        self.assertEqual(raw.decode(), TRANSCRIPT_TEXT)

    def test_transcription_requires_a_file(self):
        body, content_type = multipart({"model": "whisper-1"}, "a.wav", b"")
        status, _, _ = self.post("/v1/audio/transcriptions", body, content_type)
        self.assertEqual(status, 400)
        status, _, _ = self.post("/v1/audio/transcriptions", b"{}", "application/json")
        self.assertEqual(status, 400)


if __name__ == "__main__":
    unittest.main()
