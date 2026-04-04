#!/usr/bin/env python3
"""Sniff TNS traffic between client and dbbat proxy to understand packet flow."""
import socket, struct, threading, sys

TYPES = {1:'Connect', 2:'Accept', 3:'Refuse', 4:'Redirect', 5:'Marker', 6:'Data', 11:'Resend', 12:'Control'}

def dump_packet(label, data):
    if len(data) < 8:
        print(f"  {label}: {len(data)} bytes (too short)")
        return
    pkt_len = struct.unpack('>H', data[0:2])[0]
    pkt_type = data[4]
    tname = TYPES.get(pkt_type, f'Unk({pkt_type})')
    extra = ""
    # For Data packets, show TTC function code
    if pkt_type == 6 and len(data) > 10:
        func_code = data[10]  # offset 8 (header) + 2 (data flags)
        extra = f" ttc_func=0x{func_code:02x}"
    # Show raw header bytes for zero-length packets
    hdr_hex = data[:min(16, len(data))].hex()
    print(f"  {label}: type={tname:8s} hdr_len={pkt_len:5d} actual={len(data):5d}{extra} hdr={hdr_hex}")

def relay(src, dst, label, counter, max_dump=30):
    try:
        while True:
            data = src.recv(65536)
            if not data:
                break
            counter[0] += 1
            if counter[0] <= max_dump:
                dump_packet(f"#{counter[0]:2d} {label}", data)
            dst.sendall(data)
    except Exception as e:
        if counter[0] <= max_dump:
            print(f"  {label}: closed ({e})")
    try: src.close()
    except: pass
    try: dst.close()
    except: pass

listen_port = int(sys.argv[1]) if len(sys.argv) > 1 else 11523
target_port = int(sys.argv[2]) if len(sys.argv) > 2 else 1522

srv = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
srv.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
srv.bind(('127.0.0.1', listen_port))
srv.listen(1)
srv.settimeout(15)

print(f"Sniffing :{listen_port} -> :{target_port}")
try:
    client, addr = srv.accept()
    print(f"Client connected from {addr}")
    upstream = socket.create_connection(('127.0.0.1', target_port))
    counter = [0]
    t1 = threading.Thread(target=relay, args=(client, upstream, "C->S", counter), daemon=True)
    t2 = threading.Thread(target=relay, args=(upstream, client, "S->C", counter), daemon=True)
    t1.start(); t2.start()
    t1.join(timeout=15); t2.join(timeout=1)
    print(f"\nTotal: {counter[0]} packets")
except socket.timeout:
    print("No connection received")
finally:
    srv.close()
