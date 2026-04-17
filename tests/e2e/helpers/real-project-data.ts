/**
 * Real project configuration for integration tests.
 * Each project has its own path, language, and test configuration.
 *
 * Paths are loaded from environment variables to avoid committing local paths.
 * Set these before running tests:
 *   MAESTRO_TEST_PATH_MCP_TEST=/path/to/mcp_test
 *   MAESTRO_TEST_PATH_X_BLOG=/path/to/x-blog-serverless
 *   MAESTRO_TEST_PATH_JCAI=/path/to/jcai
 *   MAESTRO_TEST_PATH_JIUXI=/path/to/jiuxi
 */

export interface RealProject {
  name: string;
  path: string;
  language: string;
  gitInitialized: boolean;
  /** Project config passed to register_project API */
  registerConfig: {
    name: string;
    workspace_path: string;
    description?: string;
    config?: any;
  };
}

function envPath(key: string, fallback: string): string {
  return process.env[key] || fallback;
}

export const REAL_PROJECTS: Record<string, RealProject> = {
  mcp_test: {
    name: 'mcp_test',
    path: envPath('MAESTRO_TEST_PATH_MCP_TEST', ''),
    language: 'none',
    gitInitialized: true,
    registerConfig: {
      name: 'mcp_test',
      workspace_path: envPath('MAESTRO_TEST_PATH_MCP_TEST', ''),
      description: 'Micro project for Git core operation testing',
    },
  },
  x_blog: {
    name: 'x-blog-serverless',
    path: envPath('MAESTRO_TEST_PATH_X_BLOG', ''),
    language: 'typescript',
    gitInitialized: false, // needs init before test
    registerConfig: {
      name: 'x-blog-serverless',
      workspace_path: envPath('MAESTRO_TEST_PATH_X_BLOG', ''),
      description: 'Small Next.js project for zero-trust validation testing',
      config: {
        default_test_command: 'npm run build',
      },
    },
  },
  jcai: {
    name: 'jcai',
    path: envPath('MAESTRO_TEST_PATH_JCAI', ''),
    language: 'java',
    gitInitialized: true,
    registerConfig: {
      name: 'jcai',
      workspace_path: envPath('MAESTRO_TEST_PATH_JCAI', ''),
      description: 'Medium Java multi-module project for multi-agent testing',
      config: {
        default_test_command: 'echo ok', // Don't actually run Gradle
      },
    },
  },
  jiuxi: {
    name: 'jiuxi',
    path: envPath('MAESTRO_TEST_PATH_JIUXI', ''),
    language: 'java',
    gitInitialized: true, // sub-projects each have their own git
    registerConfig: {
      name: 'jiuxi',
      workspace_path: envPath('MAESTRO_TEST_PATH_JIUXI', ''),
      description: 'Large multi-repo workspace for real-time testing',
    },
  },
};

/** Get sub-project paths for multi-repo testing */
export function getSubProjects(): RealProject[] {
  const basePath = envPath('MAESTRO_TEST_PATH_JIUXI', '');
  return [
    {
      name: 'trading-signal',
      path: basePath ? `${basePath}/trading-signal` : '',
      language: 'java',
      gitInitialized: true,
      registerConfig: {
        name: 'trading-signal',
        workspace_path: basePath ? `${basePath}/trading-signal` : '',
        description: 'Trading signal project',
      },
    },
    {
      name: 'ws-sdk',
      path: basePath ? `${basePath}/ws-sdk` : '',
      language: 'java',
      gitInitialized: true,
      registerConfig: {
        name: 'ws-sdk',
        workspace_path: basePath ? `${basePath}/ws-sdk` : '',
        description: 'WebSocket SDK project',
      },
    },
    {
      name: 'frontend',
      path: basePath ? `${basePath}/frontend` : '',
      language: 'typescript',
      gitInitialized: false,
      registerConfig: {
        name: 'frontend',
        workspace_path: basePath ? `${basePath}/frontend` : '',
        description: 'Frontend project (possibly empty)',
      },
    },
  ];
}
