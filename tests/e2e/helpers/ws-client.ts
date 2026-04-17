import WebSocket from 'ws';

export interface WSEvent {
  type: string;
  project_id: string;
  payload: any;
  timestamp: string;
}

export class TestWSClient {
  private ws: WebSocket | null = null;
  private events: WSEvent[] = [];
  private connected = false;

  async connect(baseUrl: string, projectId: string): Promise<void> {
    return new Promise((resolve, reject) => {
      const wsUrl = `${baseUrl}/api/v1/projects/${projectId}/ws`;
      this.ws = new WebSocket(wsUrl);

      this.ws.on('open', () => {
        this.connected = true;
        resolve();
      });

      this.ws.on('message', (data: WebSocket.Data) => {
        const text = data.toString();
        // Messages may be newline-delimited batches
        const lines = text.split('\n').filter((l) => l.trim());
        for (const line of lines) {
          try {
            const event = JSON.parse(line) as WSEvent;
            this.events.push(event);
          } catch {
            // Ignore non-JSON messages (pings, etc.)
          }
        }
      });

      this.ws.on('error', (err) => {
        if (!this.connected) {
          reject(err);
        }
      });

      this.ws.on('close', () => {
        this.connected = false;
      });

      // Timeout after 5 seconds
      setTimeout(() => {
        if (!this.connected) {
          reject(new Error('WebSocket connection timeout'));
        }
      }, 5000);
    });
  }

  getEvents(): WSEvent[] {
    return [...this.events];
  }

  getEventsByType(type: string): WSEvent[] {
    return this.events.filter((e) => e.type === type);
  }

  waitForEvent(type: string, timeoutMs = 5000): Promise<WSEvent> {
    return new Promise((resolve, reject) => {
      // Check existing events first
      const existing = this.events.find((e) => e.type === type);
      if (existing) {
        resolve(existing);
        return;
      }

      const timer = setTimeout(() => {
        reject(new Error(`Timeout waiting for event type "${type}". Got: ${JSON.stringify(this.events.map((e) => e.type))}`));
      }, timeoutMs);

      const interval = setInterval(() => {
        const event = this.events.find((e) => e.type === type);
        if (event) {
          clearTimeout(timer);
          clearInterval(interval);
          resolve(event);
        }
      }, 100);
    });
  }

  clearEvents(): void {
    this.events = [];
  }

  isConnected(): boolean {
    return this.connected;
  }

  disconnect(): Promise<void> {
    return new Promise((resolve) => {
      if (!this.ws) {
        resolve();
        return;
      }
      this.ws.on('close', () => resolve());
      this.ws.close();
    });
  }
}
