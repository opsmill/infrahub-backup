import type {SidebarsConfig} from '@docusaurus/plugin-content-docs';

const sidebars: SidebarsConfig = {
  infrahubOpsSidebar: [
    'readme',
    {
      type: 'category',
      label: 'Tutorials',
      items: [
        'tutorials/getting-started',
      ],
    },
    {
      type: 'category',
      label: 'How-to Guides',
      items: [
        'guides/install',
        {
          type: 'category',
          label: 'Infrahub Backup',
          items: [
            'guides/backup-instance',
            'guides/restore-backup',
            'guides/kubernetes-backup',
            'guides/kubernetes-restore',
          ],
        },
        {
          type: 'category',
          label: 'Infrahub Collect',
          items: [
            'guides/collect-troubleshooting-bundle',
          ],
        },
      ],
    },
    {
      type: 'category',
      label: 'Reference',
      items: [
        'reference/commands',
        'reference/configuration',
      ],
    },
  ]
};

export default sidebars;
