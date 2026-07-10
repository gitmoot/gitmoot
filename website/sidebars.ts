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
        'concepts/agent-memory',
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
        'workflows/coordinator-recipes-workflow',
        'workflows/cockpit-orchestrate-workflow',
        'workflows/parallel-jobs-workflow',
        'workflows/heartbeat-schedules-workflow',
        'workflows/pipelines-workflow',
        'workflows/chat-workflow',
        'workflows/skillopt-train-workflow',
      ],
    },
    {
      type: 'category',
      label: 'Dashboard',
      items: [
        'dashboard/overview',
        'dashboard/views',
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
        'reference/bridge',
        'reference/runtime-adapters',
        'reference/result-contract',
        'reference/event-stream',
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
        'release-notes/v0.8.8',
        'release-notes/v0.8.7',
        'release-notes/v0.8.6',
        'release-notes/v0.8.5',
        'release-notes/v0.8.4',
        'release-notes/v0.8.3',
        'release-notes/v0.8.2',
        'release-notes/v0.8.1',
        'release-notes/v0.8.0',
        'release-notes/v0.7.0',
        'release-notes/v0.6.0',
        'release-notes/v0.5.2',
        'release-notes/v0.5.1',
        'release-notes/v0.5.0',
        'release-notes/v0.4.2',
        'release-notes/v0.4.1',
        'release-notes/v0.4.0',
        'release-notes/v0.3.5-beta.1',
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
