import type {SidebarsConfig} from '@docusaurus/plugin-content-docs';

const sidebars: SidebarsConfig = {
  infrahubOpsSidebar: [
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
        'reference/commands',
      ],
    },
  ]
};

export default sidebars;
