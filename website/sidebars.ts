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
        'concepts/agents-templates-jobs-locks',
      ],
    },
    {
      type: 'category',
      label: 'Workflows',
      items: [
        'workflows/pr-comment-workflow',
        'workflows/planner-goal-workflow',
        'workflows/template-capture-workflow',
        'workflows/review-agent-workflow',
        'workflows/skillopt-train-workflow',
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
        'reference/result-contract',
        'reference/skillopt-exchange-contract',
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
        'release-notes/v0.3.4-beta.1',
        'release-notes/v0.3.3-beta.1',
        'release-notes/v0.3.2-beta.1',
        'release-notes/v0.3.1-beta.1',
        'release-notes/v0.3.0-beta.1',
        'release-notes/v0.2.0-beta.2',
        'release-notes/v0.2.0-beta.1',
        'release-notes/v0.1.0-beta.8',
        'release-notes/v0.1.0-beta.1',
      ],
    },
  ],
};

export default sidebars;
