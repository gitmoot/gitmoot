import type {SidebarsConfig} from '@docusaurus/plugin-content-docs';

const sidebars: SidebarsConfig = {
  docsSidebar: [
    'intro',
    {
      type: 'category',
      label: 'Getting Started',
      items: [
        'getting-started/install',
        'getting-started/quick-start',
      ],
    },
    {
      type: 'category',
      label: 'Concepts',
      items: [
        'concepts/local-first-coordination',
        'concepts/agents-presets-jobs-locks',
      ],
    },
    {
      type: 'category',
      label: 'Workflows',
      items: [
        'workflows/pr-comment-workflow',
        'workflows/planner-goal-workflow',
        'workflows/review-agent-workflow',
      ],
    },
    {
      type: 'category',
      label: 'Plugins',
      items: [
        'plugins/codex-claude',
      ],
    },
    {
      type: 'category',
      label: 'Reference',
      items: [
        'reference/cli',
        'reference/runtime-adapters',
      ],
    },
    {
      type: 'category',
      label: 'Operations',
      items: [
        'operations/troubleshooting',
        'operations/beta-smoke-tests',
        'operations/deployment',
      ],
    },
    {
      type: 'category',
      label: 'Release Notes',
      items: [
        'release-notes/v0.1.0-beta.1',
      ],
    },
  ],
};

export default sidebars;

