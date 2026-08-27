export function ErrorNotice({ message, onRetry }) {
  if (!message) return null;
  return (
    <div class="error-notice" role="alert">
      <span>{message}</span>
      {onRetry && <button type="button" onClick={onRetry}>Retry</button>}
    </div>
  );
}
