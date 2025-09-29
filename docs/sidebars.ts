import type {SidebarsConfig} from '@docusaurus/plugin-content-docs';

const sidebars: SidebarsConfig = {
  infrahubopsSidebar: [
    'readme',
    {
      type: 'category',
      label: 'Guides',
      items: [
        'guides/installation',
      ],
    },
    {
      type: 'category',
      label: 'Reference',
      items: [
        'references/commands',
      ],
    },
  ]
};

export default sidebars;
