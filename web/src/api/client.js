export class APIError extends Error {
  constructor(message, { status = 0, code = 'REQUEST_FAILED', correlationId = '' } = {}) {
    super(message);
    this.name = 'APIError';
    this.status = status;
    this.code = code;
    this.correlationId = correlationId;
  }
}

async function responseBody(response) {
  const contentType = response.headers.get('content-type') || '';
  if (!contentType.includes('application/json')) return null;
  try {
    return await response.json();
  } catch {
    return null;
  }
}

export async function apiGet(path, options = {}) {
  const response = await fetch(path, {
    method: 'GET',
    headers: { Accept: 'application/json' },
    signal: options.signal,
  });
  const body = await responseBody(response);
  if (!response.ok) {
    throw new APIError(body?.error || `Request failed with HTTP ${response.status}`, {
      status: response.status,
      code: body?.error_code || 'REQUEST_FAILED',
      correlationId: body?.correlation_id || '',
    });
  }
  return body?.data ?? null;
}

export function describeAPIError(error) {
  if (!(error instanceof APIError)) return 'The service could not be reached. Check the backend and retry.';
  const details = [error.code];
  if (error.correlationId) details.push(`correlation ${error.correlationId}`);
  return `${error.message} (${details.join(', ')})`;
}
