#!/usr/bin/env python3
"""Sniff TTC responses with full hex dump for response analysis."""
import socket, struct, threading, sys

srv = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
srv.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
srv.bind(('127.0.0.1', 11527))
srv.listen(1)
srv.settimeout(20)

counter = [0]
def relay(src, dst, label):
    try:
        while True:
            data = src.recv(65536)
            if not data: break
            counter[0] += 1
            n = counter[0]
            if n <= 40 and len(data) >= 8:
                pkt_type = data[4]
                if pkt_type == 6 and len(data) > 10:
                    ttc_func = data[10]
                    ttc = data[10:]
                    if ttc_func == 0x03 and label == 'C->S' and len(ttc) > 51 and ttc[1] == 0x5e:
                        sql_len = ttc[50]
                        sql = ttc[51:51+sql_len].decode('ascii', errors='replace')
                        print(f'\n=== #{n} C->S: ExecSQL SQL="{sql}" ===')
                    elif ttc_func == 0x10 and label == 'S->C':
                        print(f'\n=== #{n} S->C: QueryResult ({len(ttc)} bytes) ===')
                        for i in range(0, min(len(ttc), 400), 16):
                            chunk = ttc[i:i+16]
                            hex_str = ' '.join(f'{b:02x}' for b in chunk)
                            ascii_str = ''.join(chr(b) if 32 <= b < 127 else '.' for b in chunk)
                            print(f'  {i:4d}: {hex_str:48s} {ascii_str}')
                    elif ttc_func == 0x08 and label == 'S->C' and n > 14:
                        print(f'\n=== #{n} S->C: Response ({len(ttc)} bytes) ===')
                        for i in range(0, min(len(ttc), 200), 16):
                            chunk = ttc[i:i+16]
                            hex_str = ' '.join(f'{b:02x}' for b in chunk)
                            ascii_str = ''.join(chr(b) if 32 <= b < 127 else '.' for b in chunk)
                            print(f'  {i:4d}: {hex_str:48s} {ascii_str}')
            dst.sendall(data)
    except: pass

print('Sniffing :11527 -> oracle-abynonprod:1521')
client, _ = srv.accept()
upstream = socket.create_connection(('oracle-abynonprod.db.stonal.io', 1521))
t1 = threading.Thread(target=relay, args=(client, upstream, 'C->S'), daemon=True)
t2 = threading.Thread(target=relay, args=(upstream, client, 'S->C'), daemon=True)
t1.start(); t2.start()
t1.join(timeout=20)
print(f'\nTotal: {counter[0]} packets')
srv.close()
