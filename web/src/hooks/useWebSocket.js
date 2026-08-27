import { useEffect, useRef, useState } from 'preact/hooks';

export function useWebSocket(url, onMessage) {
  const wsRef = useRef(null);
  const onMessageRef = useRef(onMessage);
  const [status, setStatus] = useState('idle');
  onMessageRef.current = onMessage;

  useEffect(() => {
    if (!url) {
      setStatus('idle');
      return undefined;
    }

    let reconnectTimer;
    let stopped = false;
    const connect = () => {
      if (stopped) return;
      setStatus('connecting');
      const ws = new WebSocket(url);
      wsRef.current = ws;

      ws.onopen = () => {
        if (stopped || wsRef.current !== ws) {
          ws.close();
          return;
        }
        setStatus('connected');
      };

      ws.onmessage = (e) => {
        // The Go hub may coalesce queued events into one text frame separated
        // by newlines. Preserve every event instead of dropping the whole
        // frame when JSON.parse sees more than one document.
        String(e.data).split('\n').forEach((message) => {
          if (message.trim()) onMessageRef.current(message);
        });
      };

      ws.onclose = () => {
        if (stopped) return;
        setStatus('disconnected');
        reconnectTimer = setTimeout(connect, 3000);
      };

      ws.onerror = () => {
        if (stopped || wsRef.current !== ws) return;
        setStatus('error');
        ws.close();
      };
    };

    connect();
    return () => {
      stopped = true;
      clearTimeout(reconnectTimer);
      if (wsRef.current) wsRef.current.close();
    };
  }, [url]);

  return status;
}
