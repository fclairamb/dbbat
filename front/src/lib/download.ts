// downloadBlob hands a Blob to the browser's native "save file" flow.
//
// There is no <a href> to the API endpoint that would work here: downloads
// need the same bearer-token auth as every other request, so the caller
// fetches the blob through the authenticated API client first and this
// helper only deals with getting bytes already in memory onto disk.
export function downloadBlob(blob: Blob, filename: string): void {
  const url = URL.createObjectURL(blob);

  try {
    const link = document.createElement("a");
    link.href = url;
    link.download = filename;
    document.body.appendChild(link);
    link.click();
    document.body.removeChild(link);
  } finally {
    URL.revokeObjectURL(url);
  }
}
