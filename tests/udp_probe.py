#!/usr/bin/env python3
"""Run in the test client's network namespace; no external Python packages."""
import ipaddress, os, socket, struct, sys

def exact(s, n):
    data = b''
    while len(data) < n:
        part = s.recv(n - len(data))
        if not part:
            raise RuntimeError('SOCKS closed')
        data += part
    return data

def probe(proxy, family):
    dest = '1.1.1.1' if family == 4 else '2606:4700:4700::1111'
    ident = os.urandom(2)
    query = ident + struct.pack('!HHHHH', 0x100, 1, 0, 0, 0) + b'\x07example\x03com\x00' + struct.pack('!HH', 1, 1)
    control = None
    try:
        if proxy != '-':
            host, port = proxy.rsplit(':', 1)
            control = socket.create_connection((host, int(port)), 10)
            control.settimeout(10)
            control.sendall(b'\x05\x01\x00')
            assert exact(control, 2) == b'\x05\x00'
            control.sendall(b'\x05\x03\x00\x01' + b'\0' * 6)
            h = exact(control, 4)
            assert h[1] == 0, h
            if h[3] == 1:
                bound = socket.inet_ntoa(exact(control, 4))
            elif h[3] == 4:
                bound = socket.inet_ntop(socket.AF_INET6, exact(control, 16))
            else:
                bound = exact(control, exact(control, 1)[0]).decode()
            port = struct.unpack('!H', exact(control, 2))[0]
            a = ipaddress.ip_address(dest)
            packet = b'\0\0\0' + bytes([1 if family == 4 else 4]) + a.packed + struct.pack('!H', 53) + query
            target = (bound, port)
            af = socket.AF_INET6 if ':' in bound else socket.AF_INET
        else:
            packet, target = query, (dest, 53)
            af = socket.AF_INET if family == 4 else socket.AF_INET6
        with socket.socket(af, socket.SOCK_DGRAM) as s:
            s.settimeout(15)
            s.sendto(packet, target)
            reply = s.recv(4096)
        if proxy != '-':
            assert reply[:3] == b'\0\0\0'
            at = reply[3]
            reply = reply[10 if at == 1 else 22 if at == 4 else 7 + reply[4]:]
        assert reply[:2] == ident and reply[3] & 15 == 0 and struct.unpack('!H', reply[6:8])[0] > 0
        print('UDP PASS', 'SOCKS' if proxy != '-' else 'direct', family)
    finally:
        if control:
            control.close()

if __name__ == '__main__':
    probe(sys.argv[1], int(sys.argv[2]))
