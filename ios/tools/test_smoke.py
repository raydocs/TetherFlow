import io
import json
import os
import socket
import subprocess
import sys
import threading
import unittest
from pathlib import Path
import smoke


class SmokeTests(unittest.TestCase):
    def test_partial_header_reader(self):
        status, headers = smoke.read_header(io.BytesIO(b"HTTP/1.1 200 OK\r\nContent-Length: 7\r\n\r\nbody"))
        self.assertEqual(status, "HTTP/1.1 200 OK")
        self.assertEqual(headers, {"content-length": "7"})

    def test_invalid_responses(self):
        for data in [b"", b"\r\n", b"oops\r\n\r\n", b"HTTP/1.1 200 OK\n\n", b"x" * 16385,
                     b"HTTP/1.1 200 OK\r\nX: 1\r\nx: 2\r\n\r\n"]:
            with self.assertRaises(ValueError):
                smoke.read_header(io.BytesIO(data))

    def diagnostic_fixture(self, corrupt=False):
        # Exercise the actual CLI over TCP; this fixture is NOT the phone.
        listener = socket.socket()
        listener.bind(("127.0.0.1", 0))
        listener.listen(1)
        listener.settimeout(8)
        port = listener.getsockname()[1]
        count = 131141
        errors = []

        def serve():
            try:
                with listener, listener.accept()[0] as client:
                    client.settimeout(8)
                    data = b""
                    while b"\r\n\r\n" not in data:
                        data += client.recv(4096)
                    self.assertIn(b"Proxy-Authorization: Bearer " + b"a" * 64, data)
                    body = bytes(i % 251 for i in range(count))
                    if corrupt:
                        body = b"\xff" + body[1:]
                    client.sendall(f"HTTP/1.1 200 OK\r\nContent-Length: {count}\r\n\r\n".encode())
                    for start in range(0, count, 7919):
                        client.sendall(body[start:start + 7919])
            except Exception as error:
                errors.append(error)

        thread = threading.Thread(target=serve, daemon=True)
        thread.start()
        result = subprocess.run([sys.executable, str(Path(smoke.__file__)), "--port", str(port),
                                 "download", "--bytes", str(count)],
                                env={**os.environ, "TF_TOKEN": "a" * 64}, text=True,
                                capture_output=True, timeout=15)
        thread.join(10)
        self.assertFalse(thread.is_alive())
        if errors:
            raise errors[0]
        return count, result

    def test_cli_verified_download(self):
        count, result = self.diagnostic_fixture()
        self.assertEqual(result.returncode, 0, result.stderr)
        output = json.loads(result.stdout)
        self.assertEqual(output["bytes"], count)
        self.assertIn("NOT-cellular", output["kind"])
        self.assertGreater(output["verified_payload_mbps"], 0)

    def test_cli_http80_connect(self):
        listener = socket.socket()
        listener.bind(("127.0.0.1", 0))
        listener.listen(1)
        listener.settimeout(8)
        port = listener.getsockname()[1]
        errors = []

        def serve():
            try:
                with listener, listener.accept()[0] as client:
                    client.settimeout(8)
                    with client.makefile("rb") as stream:
                        line = stream.readline()
                        self.assertEqual(line, b"CONNECT cdn.example:80 HTTP/1.1\r\n")
                        headers = []
                        while True:
                            line = stream.readline()
                            if line == b"\r\n":
                                break
                            self.assertTrue(line)
                            headers.append(line)
                        self.assertIn(b"Proxy-Authorization: Bearer " + b"a" * 64 + b"\r\n", headers)
                        client.sendall(b"HTTP/1.1 200 Connection Established\r\n\r\n")
                        self.assertEqual(stream.readline(), b"GET /chunk HTTP/1.1\r\n")
                        while stream.readline() != b"\r\n":
                            pass
                        client.sendall(b"HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok")
            except Exception as error:
                errors.append(error)

        thread = threading.Thread(target=serve, daemon=True)
        thread.start()
        result = subprocess.run([sys.executable, str(Path(smoke.__file__)), "--port", str(port),
                                 "connect", "--url", "http://cdn.example/chunk"],
                                env={**os.environ, "TF_TOKEN": "a" * 64}, text=True,
                                capture_output=True, timeout=15)
        thread.join(10)
        self.assertFalse(thread.is_alive())
        if errors:
            raise errors[0]
        self.assertEqual(result.returncode, 0, result.stderr)
        output = json.loads(result.stdout)
        self.assertIsNone(output["tls"])
        self.assertEqual(output["sample_bytes"], 2)
        self.assertEqual(output["http_status"], "HTTP/1.1 200 OK")

    def test_cli_detects_corruption(self):
        _, result = self.diagnostic_fixture(corrupt=True)
        self.assertEqual(result.returncode, 1)
        self.assertIn("payload mismatch", result.stderr)


if __name__ == "__main__":
    unittest.main()
