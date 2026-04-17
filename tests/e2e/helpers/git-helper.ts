import { execSync, exec } from 'child_process';
import * as fs from 'fs';
import * as path from 'path';

/**
 * Git helper for real-project E2E tests.
 * All commands use the project's workspace_path as cwd.
 * Paths are normalized to forward slashes for consistency on Windows.
 */

/** Initialize a git repo in the given directory. */
export function initGitRepo(dir: string): void {
  execSync('git init', { cwd: dir, stdio: 'pipe' });
}

/** Stage all files and create an initial commit. */
export function gitInitCommit(dir: string, message = 'init'): void {
  execSync('git add -A', { cwd: dir, stdio: 'pipe' });
  execSync(`git commit -m "${message}"`, { cwd: dir, stdio: 'pipe' });
}

/** Create a file with content in the given directory. */
export function makeFileChange(dir: string, filePath: string, content: string): void {
  const fullPath = path.join(dir, filePath);
  const fileDir = path.dirname(fullPath);
  fs.mkdirSync(fileDir, { recursive: true });
  fs.writeFileSync(fullPath, content, 'utf-8');
}

/** Stage and commit changes in a git repo. */
export function gitCommit(dir: string, message: string): string {
  execSync('git add -A', { cwd: dir, stdio: 'pipe' });
  execSync(`git commit -m "${message}"`, { cwd: dir, stdio: 'pipe' });
  return gitRevParse(dir, 'HEAD');
}

/** Get the HEAD commit SHA. */
export function gitRevParse(dir: string, ref: string): string {
  return execSync(`git rev-parse ${ref}`, { cwd: dir, encoding: 'utf-8' }).trim();
}

/** List git worktrees as an array of objects. */
export function getWorktreeList(dir: string): Array<{ path: string; branch: string; commit: string }> {
  const output = execSync('git worktree list --porcelain', { cwd: dir, encoding: 'utf-8' });
  const worktrees: Array<{ path: string; branch: string; commit: string }> = [];
  let current: Partial<{ path: string; branch: string; commit: string }> = {};

  for (const line of output.split('\n')) {
    if (line.startsWith('worktree ')) {
      if (current.path) worktrees.push(current as any);
      current = { path: line.slice('worktree '.length) };
    } else if (line.startsWith('HEAD ')) {
      current.commit = line.slice('HEAD '.length);
    } else if (line.startsWith('branch ')) {
      current.branch = line.slice('branch '.length);
    }
  }
  if (current.path) worktrees.push(current as any);
  return worktrees;
}

/** List git branches (local). */
export function getBranchList(dir: string): string[] {
  const output = execSync('git branch --list', { cwd: dir, encoding: 'utf-8' });
  return output
    .split('\n')
    .map(b => b.trim().replace(/^\* /, ''))
    .filter(Boolean);
}

/** Check if a file or directory exists. */
export function pathExists(p: string): boolean {
  return fs.existsSync(p);
}

/** Remove all worktrees associated with task branches under the given main repo. */
export function cleanupWorktrees(dir: string): void {
  try {
    const worktrees = getWorktreeList(dir);
    for (const wt of worktrees) {
      if (wt.branch && wt.branch.includes('task/')) {
        try {
          execSync(`git worktree remove --force "${wt.path}"`, { cwd: dir, stdio: 'pipe' });
        } catch {
          try { fs.rmSync(wt.path, { recursive: true, force: true }); } catch {}
        }
        try {
          execSync(`git branch -D "${wt.branch}"`, { cwd: dir, stdio: 'pipe' });
        } catch {}
      }
    }
  } catch {
    // Ignore errors during cleanup
  }
}

/** Get git diff --name-only for a worktree compared to base commit. */
export function getChangedFiles(dir: string, baseCommit: string): string[] {
  try {
    const output = execSync(`git diff --name-only ${baseCommit}..HEAD`, {
      cwd: dir,
      encoding: 'utf-8',
    });
    return output.split('\n').filter(Boolean);
  } catch {
    return [];
  }
}

/** Get the git log as an array of one-line entries. */
export function gitLog(dir: string, count = 10): string[] {
  try {
    const output = execSync(`git log --oneline -${count}`, { cwd: dir, encoding: 'utf-8' });
    return output.split('\n').filter(Boolean);
  } catch {
    return [];
  }
}

/** Run an arbitrary git command in the given directory. */
export function git(dir: string, args: string): string {
  return execSync(`git ${args}`, { cwd: dir, encoding: 'utf-8' }).trim();
}
